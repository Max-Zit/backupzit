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
			COALESCE(array_agg(DISTINCT j.name) FILTER (WHERE j.name IS NOT NULL), '{}'),
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
	return ok && a.Online() && !a.Recovery && !versionLess(v, [3]int{0, 42, 7})
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
	a, err := s.store.GetAgent(r.Context(), id)
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
	a, err := s.store.GetAgent(r.Context(), id)
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
	if !o.valid() {
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
	return "Agent removed. Its backups remain on the storage target.", nil
}

// completeAgentRemoval deletes the agent (with its jobs and history) and
// starts the clean-up of its backups in the background.
func (s *Server) completeAgentRemoval(ctx context.Context, a Agent, backups, by, remote string) error {
	repos, err := s.store.agentRepos(ctx, a.ID)
	if err != nil {
		return err
	}
	if err := s.store.DeleteAgent(ctx, a.ID); err != nil {
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
			detail := s.cleanupRepo(cctx, x, backups)
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
func (s *Server) cleanupRepo(ctx context.Context, x agentRepo, mode string) string {
	if x.Local() {
		return "not deleted: the storage is a disk of the agent's computer"
	}
	t, err := s.store.GetTarget(ctx, x.TargetID)
	if err != nil {
		return "not deleted: the storage target was removed"
	}
	if t.SFTPKey != "" {
		return "not deleted: the console cannot use SSH key sign-in to the storage; delete the folder there"
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
		return "not deleted: " + err.Error()
	}
	defer be.Close()
	if mode == removeAllBackups {
		return deleteRepository(ctx, be)
	}
	r, err := repo.Open(ctx, be, repo.Password(t.RecoveryKey))
	if err != nil {
		return "not deleted: " + err.Error()
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
// logs it needs) and frees the space of the others.
func keepNewestBackups(ctx context.Context, r *repo.Repository) string {
	lock, err := r.Lock(ctx, true, 2*time.Minute)
	if err != nil {
		return "not deleted: " + err.Error()
	}
	defer lock.Unlock()
	sns, err := r.ListSnapshots(ctx)
	if err != nil {
		return "not deleted: " + err.Error()
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
	for _, g := range groups {
		for _, sn := range repo.RetentionRemove(g, repo.RetentionPolicy{KeepLast: 1}) {
			if err := r.RemoveSnapshot(ctx, sn.ID); err != nil {
				return fmt.Sprintf("stopped after %d backups: %v", removed, err)
			}
			removed++
		}
	}
	if removed == 0 {
		return "nothing to delete: only the newest backups were left"
	}
	st, err := r.Prune(ctx, repo.PruneOptions{})
	if err != nil {
		return fmt.Sprintf("deleted %d older backups; freeing the space failed: %v", removed, err)
	}
	return fmt.Sprintf("kept the newest backup of each job, deleted %d older backups, freed %s", removed, humanBytes(st.BytesFreed))
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
