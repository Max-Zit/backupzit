package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/max-zit/backupzit/internal/api"
)

// VMware connections through the proxy agent (V-3, #51): the proxy checks
// the credentials and reads the VM list, version and license (vmware-probe
// runs), and reports the certificate and SSH key it saw; the console pins
// them the first time and stores the VM list. The console then needs no
// route to the host, like the backups and restores, which always ran on the
// proxy.

// minProbeAgent is the first agent version that runs vmware-probe.
var minProbeAgent = [3]int{0, 56, 0}

// errProbeQueued: a check of the host is queued or running already.
var errProbeQueued = errors.New("the proxy agent checks the host already")

// probeAgent checks that the host's proxy can run a check.
func (s *Store) probeAgent(ctx context.Context, h VMwareHost) (Agent, error) {
	if h.ProxyAgentID == nil {
		return Agent{}, errors.New("choose a proxy agent: through the proxy, it connects to the host instead of the console")
	}
	a, err := s.GetAgent(ctx, *h.ProxyAgentID)
	if err != nil || a.RemovedAt != nil {
		return Agent{}, errors.New("the proxy agent was removed")
	}
	if v, ok := parseVersion(a.Version); !ok || versionLess(v, minProbeAgent) {
		return Agent{}, fmt.Errorf("the proxy agent %s runs %s: connecting through the proxy needs agent 0.56.0 or newer (update it on this page)", a.Name(), a.Version)
	}
	return a, nil
}

// queueVMwareProbe queues a check of the host on its proxy (one at a time).
func (s *Store) queueVMwareProbe(ctx context.Context, h VMwareHost, trigger string) (int64, error) {
	a, err := s.probeAgent(ctx, h)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.db.QueryRow(ctx, `INSERT INTO runs(agent_id, kind, trigger, repo_url, notified, chat_notified, vmware_host_id, paths)
		SELECT $1,'vmware-probe',$3,'',true,true,$2,'{}'
		WHERE NOT EXISTS (SELECT 1 FROM runs WHERE vmware_host_id=$2 AND kind='vmware-probe' AND status IN ('queued','running'))
		RETURNING id`, a.ID, h.ID, trigger).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errProbeQueued
	}
	return id, err
}

// applyVMwareProbe stores what the proxy saw: the VM list, version and
// license, and pins the certificate and SSH key the first time. A host
// that presents another certificate than the pinned one is refused by the
// proxy itself; its error names the new fingerprint for Accept.
func (s *Store) applyVMwareProbe(ctx context.Context, run Run, res api.RunResult) error {
	h, err := s.vmwareHostOfRun(ctx, run.ID)
	if err != nil || h == nil || !h.ViaProxy {
		return err // removed, or switched to direct connections meanwhile
	}
	if res.Status != api.StatusSuccess {
		return s.saveVMwareInventory(ctx, h.ID, nil, "", h.APIWrites, h.SSHOK, "", "Through the proxy agent: "+res.Message)
	}
	var p api.VMwareProbe
	if json.Unmarshal(res.Details, &p) != nil || len(p.Inventory) == 0 || p.Thumbprint == "" {
		return s.saveVMwareInventory(ctx, h.ID, nil, "", h.APIWrites, h.SSHOK, "", "Through the proxy agent: the agent sent no VM list")
	}
	switch {
	case h.Thumbprint == "":
		// First contact: pinned from now on, as when the console connects
		// itself when a host is added.
		tag, err := s.db.Exec(ctx, `UPDATE vmware_hosts SET thumbprint=$2 WHERE id=$1 AND thumbprint=''`, h.ID, p.Thumbprint)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			_, _ = s.AppendAudit(ctx, "", "vmware.certificate", fmt.Sprintf("%s (%s): certificate %s pinned, as the proxy agent saw it", h.Name, h.Address, p.Thumbprint), "")
		}
	case p.Thumbprint != h.Thumbprint:
		return s.saveVMwareInventory(ctx, h.ID, nil, "", h.APIWrites, h.SSHOK, "",
			fmt.Sprintf("Through the proxy agent: the host's certificate changed (%s, expected %s)", p.Thumbprint, h.Thumbprint))
	}
	msg := ""
	if p.SSHError != "" {
		msg = "SSH (through the proxy agent): " + p.SSHError
	}
	if err := s.saveVMwareInventory(ctx, h.ID, p.Inventory, p.Version, p.APIWrites, p.SSHError == "", p.SSHHostKey, msg); err != nil {
		return err
	}
	return s.renumberVMwareJobs(ctx, h.ID, p.Inventory)
}

// claimProbe hands a busy agent a queued check of a VMware host or a
// storage connection test (#63): it takes a few seconds and must not wait
// for hours behind a long backup.
func (s *Store) claimProbe(ctx context.Context, agentID int64) (*Run, error) {
	var id int64
	err := s.db.QueryRow(ctx, `UPDATE runs SET status='running', started_at=now()
		WHERE id = (SELECT id FROM runs WHERE agent_id=$1 AND status='queued' AND kind IN ('vmware-probe','target-test') ORDER BY queued_at LIMIT 1 FOR UPDATE SKIP LOCKED)
		RETURNING id`, agentID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r, err := s.GetRun(ctx, id)
	return &r, err
}

// pruneProbeRuns deletes finished checks older than a day: they run every
// 10 minutes per host and show nowhere.
func (s *Store) pruneProbeRuns(ctx context.Context) {
	_, _ = s.db.Exec(ctx, `DELETE FROM runs WHERE kind='vmware-probe' AND status NOT IN ('queued','running') AND COALESCE(finished_at, queued_at) < now() - interval '1 day'`)
}

// waitProbe waits a little for a check the user just asked for, so the
// page can show the result at once when the proxy is quick.
func (s *Store) waitProbe(ctx context.Context, runID int64, d time.Duration) (Run, bool) {
	deadline := time.Now().Add(d)
	for {
		r, err := s.GetRun(ctx, runID)
		if err == nil && r.FinishedAt != nil {
			return r, true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return r, false
		}
		select {
		case <-ctx.Done():
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// handleVMwareConnection serves POST /vmware/{id}/connection: through the
// proxy agent (via_proxy=on) or directly from the console.
func (s *Server) handleVMwareConnection(w http.ResponseWriter, r *http.Request, _ string) {
	id, _ := pathID(r)
	h, err := s.store.GetVMwareHost(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	l := requestLanguage(r)
	on := r.FormValue("via_proxy") == "on"
	if on {
		if _, err := s.store.probeAgent(r.Context(), h); err != nil {
			redirectErr(w, r, "/agents#vmware", fmt.Errorf("%s: %v", h.Name, err))
			return
		}
	}
	if _, err := s.store.db.Exec(r.Context(), `UPDATE vmware_hosts SET via_proxy=$2 WHERE id=$1`, id, on); err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, "vmware.connection", "%s: through the proxy agent %v", h.Name, on)
	h.ViaProxy = on
	if !on {
		if err := s.store.RefreshVMwareHost(r.Context(), h); err != nil {
			redirectErr(w, r, "/agents#vmware", errors.New(l.T("%s: the console connects to the host itself now, but it cannot reach it: %v", h.Name, err)))
			return
		}
		redirectMsg(w, r, "/agents#vmware", l.T("%s: the console connects to the host itself.", h.Name))
		return
	}
	_, _ = s.store.queueVMwareProbe(r.Context(), h, "manual")
	redirectMsg(w, r, "/agents#vmware", l.T("%s: the proxy agent connects to the host now; the console needs no route to it.", h.Name))
}

// createVMwareViaProxy adds a host the console does not reach itself: the
// proxy checks the address and password, and the console pins the
// certificate and SSH key the proxy saw. The host is stored at once; a
// wrong password shows on the Agents page as the error of the check.
func (s *Server) createVMwareViaProxy(w http.ResponseWriter, r *http.Request, h VMwareHost) {
	a, err := s.store.probeAgent(r.Context(), h)
	if err != nil {
		redirectErr(w, r, "/agents#vmware", err)
		return
	}
	if agentOlder(a, minFingerprintProbe) {
		// The proxy cannot read the fingerprints alone yet: the first
		// connection pins them, as before 0.62.
		s.addVMwareViaProxy(w, r, h)
		return
	}
	// B3: the proxy first reads what the host presents, without signing
	// in; the password goes out only after the user confirmed it.
	runID, err := s.store.queueFingerprintProbe(r.Context(), a.ID, h.Address)
	if err != nil {
		redirectErr(w, r, "/agents#vmware", err)
		return
	}
	run, done := s.store.waitProbe(r.Context(), runID, 25*time.Second)
	if !done {
		redirectErr(w, r, "/agents#vmware", errors.New("the proxy agent did not answer within 25 seconds; add the host again when the agent is online"))
		return
	}
	var p api.VMwareProbe
	if run.Status != api.StatusSuccess || json.Unmarshal(run.Details, &p) != nil || p.Thumbprint == "" {
		redirectErr(w, r, "/agents#vmware", fmt.Errorf("the proxy agent could not reach %s: %s", h.Address, run.Message))
		return
	}
	s.confirmVMware(w, r, currentUser(r).Username, &pendingConnect{By: currentUser(r).Username, VMware: &h, Via: true}, p.Thumbprint, p.SSHHostKey)
}

// minFingerprintProbe is the first agent version that reads a host's
// fingerprints without signing in.
const minFingerprintProbe = "0.62.0"

// queueFingerprintProbe asks agentID to read the certificate and SSH host
// key of address (no VMware host record yet: the run's paths carry the
// address).
func (s *Store) queueFingerprintProbe(ctx context.Context, agentID int64, address string) (int64, error) {
	var id int64
	err := s.db.QueryRow(ctx, `INSERT INTO runs(agent_id, kind, trigger, repo_url, notified, chat_notified, paths)
		VALUES($1,'vmware-probe','manual','',true,true,$2) RETURNING id`, agentID, []string{fingerprintProbe, address}).Scan(&id)
	return id, err
}

// fingerprintProbe marks a probe run that only reads fingerprints.
const fingerprintProbe = "fingerprints-only"

// addVMwareViaProxy stores a host reached through its proxy agent and lets
// the agent connect (with the confirmed fingerprints, when given).
func (s *Server) addVMwareViaProxy(w http.ResponseWriter, r *http.Request, h VMwareHost) {
	l := requestLanguage(r)
	h.ViaProxy = true
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	h.RepoDir = agentRepoDir("esxi-"+h.Name, hex.EncodeToString(b))
	id, err := s.store.CreateVMwareHost(r.Context(), h)
	if err != nil {
		redirectErr(w, r, "/agents#vmware", err)
		return
	}
	h.ID = id
	s.audit(r, "vmware.create", "%s (%s) through the proxy agent", h.Name, h.Address)
	runID, err := s.store.queueVMwareProbe(r.Context(), h, "manual")
	if err != nil {
		redirectErr(w, r, "/agents#vmware", err)
		return
	}
	if run, done := s.store.waitProbe(r.Context(), runID, 20*time.Second); done {
		if run.Status != api.StatusSuccess {
			redirectErr(w, r, "/agents#vmware", errors.New(l.T("%s was added, but the proxy agent could not connect to it: %s. Correct the address or password (remove the host and add it again), or check the network between the proxy and the host.", h.Name, run.Message)))
			return
		}
		if nh, err := s.store.GetVMwareHost(r.Context(), id); err == nil {
			redirectMsg(w, r, "/agents#vmware", l.T("%s added through the proxy agent (%s). Its certificate %s is pinned.", h.Name, nh.Version, nh.Thumbprint))
			return
		}
	}
	redirectMsg(w, r, "/agents#vmware", l.T("%s added. The proxy agent checks the connection as soon as it is free; the VM list and the pinned certificate appear here then.", h.Name))
}
