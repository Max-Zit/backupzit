package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
)

// What happens to an agent's backups when the agent is removed.
const (
	removeKeepBackups = "keep" // everything stays on the storage
	removeKeepLast    = "last" // only the newest backup of each job stays
	removeAllBackups  = "all"  // the agent's repositories are deleted
)

// removalOpts are the choices of the remove dialog (also the payload of a
// four-eyes request).
type removalOpts struct {
	Uninstall bool   `json:"uninstall"`
	Backups   string `json:"backups"`
}

func (o removalOpts) valid() bool {
	return o.Backups == removeKeepBackups || o.Backups == removeKeepLast || o.Backups == removeAllBackups
}

// agentRepo is a repository holding backups made by an agent.
type agentRepo struct {
	TargetID   int64
	TargetName string
	TargetKind string
	URL        string
	Jobs       []string
	Backups    int
}

// Local reports whether the repository is on a disk of the agent's own
// computer, which the console cannot reach.
func (x agentRepo) Local() bool { return x.TargetKind == "local" || x.TargetKind == "usb" }

// agentRepos lists the repositories with backups of the agent (copies that
// copy jobs made on other storage are not included).
func (s *Store) agentRepos(ctx context.Context, agentID int64) ([]agentRepo, error) {
	rows, err := s.db.Query(ctx, `SELECT r.target_id, t.name, t.kind, r.repo_url,
			COALESCE(array_agg(DISTINCT COALESCE(j.name, NULLIF(r.job_name,''))) FILTER (WHERE COALESCE(j.name, NULLIF(r.job_name,'')) IS NOT NULL), '{}'),
			count(*) FILTER (WHERE r.status IN ('success','warning') AND r.snapshot_id<>'' AND NOT r.expired)
		FROM runs r JOIN storage_targets t ON t.id=r.target_id LEFT JOIN jobs j ON j.id=r.job_id
		WHERE r.agent_id=$1 AND r.repo_url<>'' AND r.kind IN ('backup','image-backup','vm-backup','system-backup','sql-backup','sql-log')
		GROUP BY r.target_id, t.name, t.kind, r.repo_url ORDER BY t.name, r.repo_url`, agentID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (agentRepo, error) {
		var x agentRepo
		err := r.Scan(&x.TargetID, &x.TargetName, &x.TargetKind, &x.URL, &x.Jobs, &x.Backups)
		sort.Strings(x.Jobs)
		return x, err
	})
}

// softRemoveAgent keeps a removed agent as history of its backups: queued
// runs are dropped, its jobs deleted (runs keep the job's name) and it can
// no longer sign in.
func (s *Store) softRemoveAgent(ctx context.Context, id int64) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, q := range []string{
		`UPDATE runs r SET job_name=j.name FROM jobs j WHERE r.job_id=j.id AND r.agent_id=$1`,
		`DELETE FROM runs WHERE agent_id=$1 AND status='queued'`,
		`UPDATE runs SET status='failed', message='The agent was removed.', finished_at=now() WHERE agent_id=$1 AND status='running'`,
		`UPDATE vmware_hosts SET proxy_agent_id=NULL WHERE proxy_agent_id=$1`,
		`DELETE FROM jobs WHERE agent_id=$1`,
		`DELETE FROM agent_removals WHERE agent_id=$1`,
		`UPDATE agents SET removed_at=now() WHERE id=$1`,
	} {
		if _, err := tx.Exec(ctx, q, id); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// expireSnapshots marks the runs of deleted backups.
func (s *Store) expireSnapshots(ctx context.Context, agentID int64, ids []string) {
	s.db.Exec(ctx, `UPDATE runs SET expired=true WHERE agent_id=$1 AND snapshot_id = ANY($2)`, agentID, ids)
}

// removedAgent is a removed agent kept for its backups.
type removedAgent struct {
	Agent
	Backups int
	// Successor is the current agent with the same name, which can take
	// over the backups (0 if none).
	Successor int64
}

// listRemovedAgents lists the removed agents kept for their backups.
func (s *Store) listRemovedAgents(ctx context.Context) ([]removedAgent, error) {
	rows, err := s.db.Query(ctx, `SELECT `+agentCols+`,
			(SELECT count(*) FROM runs r WHERE r.agent_id=agents.id AND r.status IN ('success','warning') AND r.snapshot_id<>'' AND NOT r.expired),
			COALESCE((SELECT n.id FROM agents n WHERE n.removed_at IS NULL AND lower(n.hostname)=lower(agents.hostname) ORDER BY n.enrolled_at DESC LIMIT 1), 0)
		FROM agents WHERE removed_at IS NOT NULL ORDER BY hostname, removed_at DESC`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (removedAgent, error) {
		var x removedAgent
		a, err := scanAgent(r, &x.Backups, &x.Successor)
		x.Agent = a
		if !a.ownRepo() {
			x.Successor = 0
		}
		return x, err
	})
}

// ownRepo reports whether the agent's repository folder is an agent's
// (backups found on storage can also be of a VMware connection or a
// Microsoft 365 tenant, which no agent continues).
func (a Agent) ownRepo() bool {
	return !strings.HasPrefix(a.RepoDir, "esxi-") && !strings.HasPrefix(a.RepoDir, "m365-")
}

// agentAny returns an agent, also a removed one.
func (s *Store) agentAny(ctx context.Context, id int64) (Agent, error) {
	a, err := scanAgent(s.db.QueryRow(ctx, `SELECT `+agentCols+` FROM agents WHERE id=$1`, id))
	return a, notFound(err)
}

// adoptBackups gives the backups of a removed agent to a current agent: its
// history moves over. A current agent without backups of its own (just
// reinstalled) also takes over the removed agent's repositories, so its jobs
// continue there, deduplicated against the old backups; one with backups
// keeps its repositories (moved reports which).
func (s *Store) adoptBackups(ctx context.Context, agentID, removedID int64) (moved bool, err error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var dir string
	if err := tx.QueryRow(ctx, `SELECT repo_dir FROM agents WHERE id=$1 AND removed_at IS NOT NULL`, removedID).Scan(&dir); err != nil {
		return false, notFound(err)
	}
	var own bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agents WHERE id=$1 AND removed_at IS NULL),
		EXISTS(SELECT 1 FROM runs WHERE agent_id=$1 AND snapshot_id<>'')`, agentID).Scan(&moved, &own); err != nil {
		return false, err
	}
	if !moved {
		return false, ErrNotFound
	}
	moved = !own
	if moved {
		// Repository folders are unique: the removed agent gives its folder up first.
		if _, err := tx.Exec(ctx, `UPDATE agents SET repo_dir=repo_dir || '.adopted-' || id WHERE id=$1`, removedID); err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, `UPDATE agents SET repo_dir=$2 WHERE id=$1`, agentID, dir); err != nil {
			return false, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET agent_id=$1 WHERE agent_id=$2`, agentID, removedID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM agents WHERE id=$1`, removedID); err != nil {
		return false, err
	}
	return moved, tx.Commit(ctx)
}

func (s *Store) setAgentRemoval(ctx context.Context, agentID, runID int64, backups, by string) error {
	_, err := s.db.Exec(ctx, `INSERT INTO agent_removals(agent_id, run_id, backups, requested_by) VALUES($1,$2,$3,$4)
		ON CONFLICT (agent_id) DO UPDATE SET run_id=$2, backups=$3, requested_by=$4, requested_at=now()`, agentID, runID, backups, by)
	return err
}

// agentRemoval is the pending removal of an agent, if any.
func (s *Store) agentRemoval(ctx context.Context, agentID int64) (runID int64, backups, by string, ok bool) {
	err := s.db.QueryRow(ctx, `SELECT run_id, backups, requested_by FROM agent_removals WHERE agent_id=$1`, agentID).Scan(&runID, &backups, &by)
	return runID, backups, by, err == nil
}

func (s *Store) clearAgentRemoval(ctx context.Context, agentID int64) {
	s.db.Exec(ctx, `DELETE FROM agent_removals WHERE agent_id=$1`, agentID)
}

func (s *Store) queueAgentUninstall(ctx context.Context, agentID int64) (int64, error) {
	var id int64
	err := s.db.QueryRow(ctx, `INSERT INTO runs(agent_id, kind, trigger, repo_url, notified)
		SELECT $1,'agent-uninstall','manual','',true
		WHERE NOT EXISTS (SELECT 1 FROM runs WHERE agent_id=$1 AND kind='agent-uninstall' AND status IN ('queued','running'))
		RETURNING id`, agentID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errors.New("the agent is already uninstalling")
	}
	return id, err
}

// canUninstall reports whether the agent can remove itself: online, not a
// recovery system, and a version that knows the command.
func canUninstall(a Agent) bool {
	v, ok := parseVersion(a.Version)
	return ok && a.RemovedAt == nil && a.Online() && !a.Recovery && !versionLess(v, [3]int{0, 42, 7})
}

// handleAgentAdopt serves POST /agents/{id}/adopt: the agent of a
// reinstalled computer takes over the backups of the removed agent with
// the same name.
func (s *Server) handleAgentAdopt(w http.ResponseWriter, r *http.Request, user string) {
	id, _ := pathID(r)
	a, err := s.store.GetAgent(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	old, err := s.store.agentAny(r.Context(), formID(r, "removed"))
	l := requestLanguage(r)
	if err != nil || old.RemovedAt == nil || !strings.EqualFold(old.Hostname, a.Hostname) || !old.ownRepo() {
		redirectErr(w, r, "/agents", errors.New(l.T("These backups cannot be connected to this agent.")))
		return
	}
	moved, err := s.store.adoptBackups(r.Context(), a.ID, old.ID)
	if err != nil {
		redirectErr(w, r, "/agents", err)
		return
	}
	s.audit(r, "agent.adopt", "%s (agent %d) took over the backups of removed agent %d", a.Hostname, a.ID, old.ID)
	if !moved {
		redirectMsg(w, r, "/agents", l.T("The backups of the removed agent %s are connected: they appear under it. Its jobs keep their own repositories, as it already has backups.", a.Hostname))
		return
	}
	redirectMsg(w, r, "/agents", l.T("The backups of the removed agent %s are connected: they appear under it, and its jobs continue in the same repositories.", a.Hostname))
}

// agentRemoveData is the remove dialog.
type agentRemoveData struct {
	Agent        Agent
	Repos        []agentRepo
	CanUninstall bool
	Pending      bool
	ProxyJobs    []string
	Immutable    []string
}

func (s *Server) handleAgentRemovePage(w http.ResponseWriter, r *http.Request, user string) {
	id, _ := pathID(r)
	a, err := s.store.agentAny(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	repos, err := s.store.agentRepos(r.Context(), id)
	if err != nil {
		s.serverError(w, err)
		return
	}
	d := agentRemoveData{Agent: a, Repos: repos, CanUninstall: canUninstall(a)}
	_, _, _, d.Pending = s.store.agentRemoval(r.Context(), id)
	if jobs, err := s.store.ListJobs(r.Context()); err == nil {
		for _, j := range jobs {
			if j.AgentID == id && (j.Kind == JobVM || j.Kind == JobM365 || j.Kind == JobNAS) {
				d.ProxyJobs = append(d.ProxyJobs, j.Name)
			}
		}
	}
	seen := map[int64]bool{}
	for _, x := range repos {
		if seen[x.TargetID] {
			continue
		}
		seen[x.TargetID] = true
		if t, err := s.store.GetTarget(r.Context(), x.TargetID); err == nil && (t.Kind == "hardened" || t.S3LockDays > 0) {
			d.Immutable = append(d.Immutable, t.Name)
		}
	}
	s.render(w, r, "agent_remove", pageData{Title: "Remove agent", Nav: "agents", User: user, Data: d})
}

// handleAgentDelete serves POST /agents/{id}/delete with the choices of the
// remove dialog: uninstall the agent from its computer, and what happens
// to its backups.
func (s *Server) handleAgentDelete(w http.ResponseWriter, r *http.Request, user string) {
	id, _ := pathID(r)
	a, err := s.store.agentAny(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	back := fmt.Sprintf("/agents/remove/%d", id)
	o := removalOpts{Uninstall: r.FormValue("uninstall") == "on", Backups: r.FormValue("backups")}
	if o.Backups == "" {
		o.Backups = removeKeepBackups
	}
	l := requestLanguage(r)
	if !o.valid() || (a.RemovedAt != nil && o.Backups == removeKeepBackups) {
		redirectErr(w, r, back, errors.New(l.T("Choose what happens to the backups.")))
		return
	}
	if o.Backups != removeKeepBackups && !strings.EqualFold(strings.TrimSpace(r.FormValue("confirm")), a.Hostname) {
		redirectErr(w, r, back, errors.New(l.T("Type the name of the computer, %s, to delete its backups.", a.Hostname)))
		return
	}
	if s.needsApproval(w, r, "/agents", "agent.delete", id, a.Hostname, o) {
		return
	}
	msg, err := s.startAgentRemoval(r.Context(), a, o, user, s.guard.ClientIP(r))
	if err != nil {
		redirectErr(w, r, "/agents", err)
		return
	}
	redirectMsg(w, r, "/agents", l.T(msg))
}

// startAgentRemoval removes an agent now, or first lets it uninstall
// itself (it is removed when it reports). It returns the message for the
// user (an English catalog key).
func (s *Server) startAgentRemoval(ctx context.Context, a Agent, o removalOpts, by, remote string) (string, error) {
	if o.Uninstall && canUninstall(a) {
		rid, err := s.store.queueAgentUninstall(ctx, a.ID)
		if err != nil {
			return "", err
		}
		if err := s.store.setAgentRemoval(ctx, a.ID, rid, o.Backups, by); err != nil {
			return "", err
		}
		s.store.AppendAudit(ctx, by, "agent.uninstall", fmt.Sprintf("%s (agent %d), backups: %s", a.Hostname, a.ID, o.Backups), remote)
		return "The agent uninstalls itself from the computer and is then removed here.", nil
	}
	if err := s.completeAgentRemoval(ctx, a, o.Backups, by, remote); err != nil {
		return "", err
	}
	switch {
	case o.Backups != removeKeepBackups:
		return "Agent removed. Its backups are being deleted on the storage; the audit log shows the result.", nil
	case o.Uninstall:
		return "Agent removed. It was not online, so it is still installed on the computer: uninstall it there.", nil
	}
	return "Agent removed. Its backups stay in Activity and can be restored to another agent.", nil
}

// completeAgentRemoval removes the agent and starts the clean-up of its
// backups in the background. An agent whose backups are kept (all or the
// newest) stays as history: its backups remain in Activity, can be
// restored to other agents and connected to a reinstalled computer. Its
// jobs are deleted and it can no longer sign in.
func (s *Server) completeAgentRemoval(ctx context.Context, a Agent, backups, by, remote string) error {
	repos, err := s.store.agentRepos(ctx, a.ID)
	if err != nil {
		return err
	}
	if backups == removeAllBackups || len(repos) == 0 {
		err = s.store.DeleteAgent(ctx, a.ID)
	} else {
		err = s.store.softRemoveAgent(ctx, a.ID)
	}
	if err != nil {
		return err
	}
	s.store.AppendAudit(ctx, by, "agent.delete", fmt.Sprintf("%s (agent %d), backups: %s", a.Hostname, a.ID, backups), remote)
	if backups == removeKeepBackups || len(repos) == 0 {
		return nil
	}
	s.cleanups.Add(1)
	go func() {
		defer s.cleanups.Done()
		cctx, cancel := context.WithTimeout(context.Background(), 12*time.Hour)
		defer cancel()
		for _, x := range repos {
			detail, removed := s.cleanupRepo(cctx, x, backups)
			if len(removed) > 0 {
				s.store.expireSnapshots(cctx, a.ID, removed)
			}
			s.store.AppendAudit(cctx, by, "agent.backups", fmt.Sprintf("%s, %s on %s: %s", a.Hostname, x.URL, x.TargetName, detail), remote)
			s.log.Info("backups of a removed agent", "agent", a.Hostname, "repo", x.URL, "result", detail)
		}
	}()
	return nil
}

// agentUninstalled finishes a pending removal when the agent reported its
// uninstall run.
func (s *Server) agentUninstalled(ctx context.Context, a Agent, runID int64, res api.RunResult) {
	rid, backups, by, ok := s.store.agentRemoval(ctx, a.ID)
	if !ok || rid != runID {
		return
	}
	if res.Status == api.StatusFailed {
		s.store.clearAgentRemoval(ctx, a.ID)
		s.store.AppendAudit(ctx, by, "agent.uninstall", fmt.Sprintf("%s: uninstall failed (%s); the agent was not removed", a.Hostname, res.Message), "")
		return
	}
	if err := s.completeAgentRemoval(ctx, a, backups, by, ""); err != nil {
		s.log.Error("remove uninstalled agent", "agent", a.Hostname, "err", err)
	}
}

// cleanupRepo deletes backups of a removed agent in one repository and
// describes the outcome.
func (s *Server) cleanupRepo(ctx context.Context, x agentRepo, mode string) (string, []string) {
	if x.Local() {
		return "not deleted: the storage is a disk of the agent's computer", nil
	}
	t, err := s.store.GetTarget(ctx, x.TargetID)
	if err != nil {
		return "not deleted: the storage target was removed", nil
	}
	if t.SFTPKey != "" {
		return "not deleted: the console cannot use SSH key sign-in to the storage; delete the folder there", nil
	}
	s.cache.drop(x.URL)
	be, err := backend.Open(ctx, x.URL, backend.Options{
		SFTPPassword: t.SFTPPassword, SFTPHostKey: t.SFTPHostKey,
		S3AccessKey: t.S3AccessKey, S3SecretKey: t.S3SecretKey, S3Region: t.S3Region, S3LockDays: t.S3LockDays,
		SMBPassword: t.SMBPassword, SMBDomain: t.SMBDomain,
		HardenedKey: t.HardenedKey, HardenedFingerprint: t.HardenedFingerprint,
		AzureKey: t.AzureKey, AzureSAS: t.AzureSAS,
	})
	if err != nil {
		return "not deleted: " + err.Error(), nil
	}
	defer be.Close()
	if mode == removeAllBackups {
		return deleteRepository(ctx, be), nil
	}
	r, err := repo.Open(ctx, be, repo.Password(t.RecoveryKey))
	if err != nil {
		return "not deleted: " + err.Error(), nil
	}
	return keepNewestBackups(ctx, r)
}

// deleteRepository removes every object of a repository (immutable storage
// refuses what is still locked).
func deleteRepository(ctx context.Context, be backend.Backend) string {
	names, err := be.List(ctx, "")
	if err != nil {
		return "not deleted: " + err.Error()
	}
	var failed int
	var first error
	for _, n := range names {
		if err := be.Remove(ctx, n); err != nil {
			failed++
			if first == nil {
				first = err
			}
		}
	}
	if dr, ok := be.(backend.DirRemover); ok && failed == 0 {
		dr.RemoveDirs(ctx)
	}
	if failed > 0 {
		return fmt.Sprintf("deleted %d of %d files; %d could not be deleted (immutable storage keeps them until the lock expires): %v", len(names)-failed, len(names), failed, first)
	}
	return fmt.Sprintf("repository deleted (%d files)", len(names))
}

// keepNewestBackups keeps the newest backup of each job (with the database
// logs it needs), frees the space of the others and returns their IDs.
func keepNewestBackups(ctx context.Context, r *repo.Repository) (string, []string) {
	lock, err := r.Lock(ctx, true, 2*time.Minute)
	if err != nil {
		return "not deleted: " + err.Error(), nil
	}
	defer lock.Unlock()
	sns, err := r.ListSnapshots(ctx)
	if err != nil {
		return "not deleted: " + err.Error(), nil
	}
	groups := map[string][]*repo.Snapshot{}
	for _, sn := range sns {
		key := ""
		for _, t := range sn.Tags {
			if strings.HasPrefix(t, "job:") {
				key = t
			}
		}
		if key == "" { // made without a job (command line)
			key = sn.Hostname + "\x00" + strings.Join(sn.Paths, "\x00")
		}
		groups[key] = append(groups[key], sn)
	}
	var removed int
	var ids []string
	for _, g := range groups {
		for _, sn := range repo.RetentionRemove(g, repo.RetentionPolicy{KeepLast: 1}) {
			if err := r.RemoveSnapshot(ctx, sn.ID); err != nil {
				return fmt.Sprintf("stopped after %d backups: %v", removed, err), ids
			}
			removed++
			ids = append(ids, sn.ID.String())
		}
	}
	if removed == 0 {
		return "nothing to delete: only the newest backups were left", nil
	}
	st, err := r.Prune(ctx, repo.PruneOptions{})
	if err != nil {
		return fmt.Sprintf("deleted %d older backups; freeing the space failed: %v", removed, err), ids
	}
	return fmt.Sprintf("kept the newest backup of each job, deleted %d older backups, freed %s", removed, humanBytes(st.BytesFreed)), ids
}

// executeAgentDelete carries out an approved removal.
func (s *Server) executeAgentDelete(ctx context.Context, ap Approval, approver string) error {
	a, err := s.store.GetAgent(ctx, ap.ObjectID)
	if err != nil {
		return err
	}
	o := removalOpts{Backups: removeKeepBackups}
	if len(ap.Payload) > 0 {
		if err := json.Unmarshal(ap.Payload, &o); err != nil {
			return err
		}
	}
	if !o.valid() {
		o.Backups = removeKeepBackups
	}
	by := ap.RequestedBy
	if approver != "" {
		by += ", approved by " + approver
	}
	_, err = s.startAgentRemoval(ctx, a, o, by, "")
	return err
}
