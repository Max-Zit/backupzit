package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/max-zit/backupzit/internal/repo"
)

// A JobGroup is a files job for several agents: the selected ones, all of
// an operating system or all agents. The console keeps one ordinary job per
// matching agent (its members), so runs, restores and reports work as for
// any job, and agents that join later are added by the scheduler.
type JobGroup struct {
	ID         int64
	Name       string
	Scope      string // GroupAgents, GroupAll, GroupWindows, GroupLinux, GroupMacOS
	AgentIDs   []int64
	ExcludeIDs []int64
	TargetID   int64
	TargetName string
	Paths      []string
	Excludes   []string
	Schedule   string
	Retention  repo.RetentionPolicy
	Options    JobOptions
	Enabled    bool
	CreatedAt  time.Time
}

const (
	GroupAgents  = "agents"
	GroupAll     = "all"
	GroupWindows = "windows"
	GroupLinux   = "linux"
	GroupMacOS   = "macos"
)

var groupScopes = []string{GroupAgents, GroupAll, GroupWindows, GroupLinux, GroupMacOS}

// Matches reports whether the group backs up agent a.
func (g JobGroup) Matches(a Agent) bool {
	if a.Recovery || a.RemovedAt != nil {
		return false
	}
	if g.Scope == GroupAgents {
		return slices.Contains(g.AgentIDs, a.ID)
	}
	if slices.Contains(g.ExcludeIDs, a.ID) {
		return false
	}
	switch g.Scope {
	case GroupAll:
		return true
	case GroupWindows, GroupLinux, GroupMacOS:
		return agentPlatform(a) == g.Scope
	}
	return false
}

// agentPlatform is GroupWindows, GroupLinux or GroupMacOS for the
// operating system an agent reports: "Windows 11 Pro (build …)", the
// PRETTY_NAME of a Linux distribution ("Ubuntu 24.04.1 LTS") or "darwin".
func agentPlatform(a Agent) string {
	os := strings.ToLower(a.OS)
	switch {
	case os == "":
		return ""
	case strings.Contains(os, "windows"):
		return GroupWindows
	case strings.Contains(os, "darwin"), strings.Contains(os, "macos"), strings.Contains(os, "mac os"):
		return GroupMacOS
	}
	return GroupLinux
}

// member is the job of the group for one agent.
func (g JobGroup) member(agentID int64) Job {
	return Job{Kind: JobFiles, AgentID: agentID, TargetID: g.TargetID, Name: g.Name, Paths: g.Paths, Excludes: g.Excludes,
		Schedule: g.Schedule, Retention: g.Retention, Options: g.Options, Enabled: g.Enabled, GroupID: &g.ID}
}

// sameSettings reports whether a member job already has the group's settings.
func (g JobGroup) sameSettings(j Job) bool {
	return j.Name == g.Name && slices.Equal(j.Paths, g.Paths) && slices.Equal(j.Excludes, g.Excludes) &&
		j.Schedule == g.Schedule && j.Retention == g.Retention && sameJSON(j.Options, g.Options)
}

func (g *JobGroup) check() error {
	g.Name = strings.TrimSpace(g.Name)
	if g.Name == "" {
		return errors.New("enter a name")
	}
	if !slices.Contains(groupScopes, g.Scope) {
		return errors.New("choose which agents the job backs up")
	}
	if g.Scope == GroupAgents && len(g.AgentIDs) == 0 {
		return errors.New("choose at least one agent")
	}
	if g.Scope == GroupAgents {
		g.ExcludeIDs = nil
	} else {
		g.AgentIDs = nil
	}
	if g.AgentIDs == nil {
		g.AgentIDs = []int64{}
	}
	if g.ExcludeIDs == nil {
		g.ExcludeIDs = []int64{}
	}
	if len(g.Paths) == 0 {
		return errors.New("enter at least one folder")
	}
	if g.Excludes == nil {
		g.Excludes = []string{}
	}
	return g.Options.Validate()
}

const groupCols = `g.id, g.name, g.scope, g.agent_ids, g.exclude_ids, g.target_id, st.name, g.paths, g.excludes, g.schedule, g.retention, g.options, g.enabled, g.created_at`

const groupFrom = ` FROM job_groups g JOIN storage_targets st ON st.id=g.target_id`

func scanGroup(r pgx.Row) (JobGroup, error) {
	var g JobGroup
	err := r.Scan(&g.ID, &g.Name, &g.Scope, &g.AgentIDs, &g.ExcludeIDs, &g.TargetID, &g.TargetName, &g.Paths, &g.Excludes,
		&g.Schedule, &g.Retention, &g.Options, &g.Enabled, &g.CreatedAt)
	return g, err
}

func (s *Store) ListJobGroups(ctx context.Context) ([]JobGroup, error) {
	rows, err := s.db.Query(ctx, `SELECT `+groupCols+groupFrom+` ORDER BY g.name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (JobGroup, error) { return scanGroup(r) })
}

func (s *Store) GetJobGroup(ctx context.Context, id int64) (JobGroup, error) {
	g, err := scanGroup(s.db.QueryRow(ctx, `SELECT `+groupCols+groupFrom+` WHERE g.id=$1`, id))
	return g, notFound(err)
}

func (s *Store) CreateJobGroup(ctx context.Context, g JobGroup) (int64, error) {
	if err := g.check(); err != nil {
		return 0, err
	}
	if _, err := s.GetTarget(ctx, g.TargetID); err != nil {
		return 0, errors.New("unknown storage target")
	}
	var id int64
	err := s.db.QueryRow(ctx, `INSERT INTO job_groups(name, scope, agent_ids, exclude_ids, target_id, paths, excludes, schedule, retention, options, enabled)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,true) RETURNING id`,
		g.Name, g.Scope, g.AgentIDs, g.ExcludeIDs, g.TargetID, g.Paths, g.Excludes, g.Schedule, g.Retention, g.Options).Scan(&id)
	return id, err
}

// UpdateJobGroup saves the settings of a group (not its storage target);
// the members follow with the next reconcileGroups.
func (s *Store) UpdateJobGroup(ctx context.Context, g JobGroup) error {
	if err := g.check(); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `UPDATE job_groups SET name=$2, scope=$3, agent_ids=$4, exclude_ids=$5, paths=$6, excludes=$7, schedule=$8, retention=$9, options=$10 WHERE id=$1`,
		g.ID, g.Name, g.Scope, g.AgentIDs, g.ExcludeIDs, g.Paths, g.Excludes, g.Schedule, g.Retention, g.Options)
	return err
}

func (s *Store) SetJobGroupEnabled(ctx context.Context, id int64, enabled bool) error {
	if _, err := s.db.Exec(ctx, `UPDATE job_groups SET enabled=$2 WHERE id=$1`, id, enabled); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `UPDATE jobs SET enabled=$2, last_scheduled_at=now() WHERE group_id=$1`, id, enabled)
	return err
}

// DeleteJobGroup deletes the group and its member jobs; their backups stay.
func (s *Store) DeleteJobGroup(ctx context.Context, id int64) error {
	_, err := s.db.Exec(ctx, `DELETE FROM job_groups WHERE id=$1`, id)
	return err
}

func (s *Store) groupMembers(ctx context.Context, id int64) ([]Job, error) {
	rows, err := s.db.Query(ctx, `SELECT `+jobCols+jobFrom+` WHERE j.group_id=$1 ORDER BY a.hostname`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Job, error) { return scanJob(r) })
}

// groupChange is what reconcileGroups did, for the audit log.
type groupChange struct {
	Group, Agent string
	Added        bool
}

// reconcileGroups gives every matching agent its member job, brings the
// members to the group's settings and removes members of agents the group
// no longer covers (their backups stay). An agent that cannot run the job
// yet (e.g. too old for {users}) is skipped and listed on the group page.
func (s *Store) reconcileGroups(ctx context.Context) ([]groupChange, error) {
	groups, err := s.ListJobGroups(ctx)
	if err != nil || len(groups) == 0 {
		return nil, err
	}
	agents, err := s.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	var changes []groupChange
	for _, g := range groups {
		members, err := s.groupMembers(ctx, g.ID)
		if err != nil {
			return changes, err
		}
		have := map[int64]Job{}
		for _, m := range members {
			have[m.AgentID] = m
		}
		for _, a := range agents {
			m, ok := have[a.ID]
			switch {
			case !g.Matches(a):
			case !ok:
				if _, err := s.CreateJob(ctx, g.member(a.ID)); err == nil {
					changes = append(changes, groupChange{g.Name, a.Hostname, true})
				}
			case !g.sameSettings(m):
				u := g.member(a.ID)
				u.ID, u.Enabled = m.ID, m.Enabled
				_ = s.UpdateJob(ctx, u) // an agent that cannot run the new settings keeps the old ones
			}
		}
		for _, m := range members {
			if i := slices.IndexFunc(agents, func(a Agent) bool { return a.ID == m.AgentID }); i < 0 || !g.Matches(agents[i]) {
				if err := s.DeleteJob(ctx, m.ID); err != nil {
					return changes, err
				}
				changes = append(changes, groupChange{g.Name, m.Hostname, false})
			}
		}
	}
	return changes, nil
}

// syncGroups runs reconcileGroups and writes its changes to the audit log.
func (s *Store) syncGroups(ctx context.Context) error {
	changes, err := s.reconcileGroups(ctx)
	for _, c := range changes {
		what := "added"
		if !c.Added {
			what = "removed (its backups stay)"
		}
		s.AppendAudit(ctx, "", "group.member", fmt.Sprintf("group job %s: %s %s", c.Group, c.Agent, what), "")
	}
	return err
}

// groupMemberRow is one agent on the group page.
type groupMemberRow struct {
	Agent Agent
	Job   *Job
	Issue string // why the agent has no member job
}

// groupRows lists the agents of a group with their member jobs, and the
// matching agents that have none with the reason.
func (s *Store) groupRows(ctx context.Context, g JobGroup) ([]groupMemberRow, error) {
	agents, err := s.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	members, err := s.groupMembers(ctx, g.ID)
	if err != nil {
		return nil, err
	}
	var rows []groupMemberRow
	for _, a := range agents {
		if !g.Matches(a) {
			continue
		}
		row := groupMemberRow{Agent: a}
		if i := slices.IndexFunc(members, func(j Job) bool { return j.AgentID == a.ID }); i >= 0 {
			row.Job = &members[i]
		} else {
			j := g.member(a.ID)
			if err := s.checkJob(ctx, &j); err != nil {
				row.Issue = err.Error()
			} else {
				row.Issue = "added within a minute"
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// ---- web

// groupFromForm reads the agent choice of a job form: agent_id
// "group:<scope>", with the agents (group_agents) for the "agents" scope or
// the agents left out (group_exclude) for the others.
func groupFromForm(r *http.Request, g JobGroup) JobGroup {
	g.Scope = strings.TrimPrefix(r.FormValue("agent_id"), "group:")
	if v := r.FormValue("scope"); v != "" {
		g.Scope = v
	}
	g.AgentIDs, g.ExcludeIDs = formIDs(r, "group_agents"), formIDs(r, "group_exclude")
	g.Name = r.FormValue("name")
	g.Paths, g.Excludes = lines(r.FormValue("paths")), lines(r.FormValue("excludes"))
	return g
}

func formIDs(r *http.Request, name string) []int64 {
	var ids []int64
	for _, v := range r.Form[name] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids
}

// handleGroupCreate creates a group job from the new job form.
func (s *Server) handleGroupCreate(w http.ResponseWriter, r *http.Request, sched string) {
	if k := r.FormValue("kind"); k != JobFiles {
		redirectErr(w, r, "/jobs", errors.New("a job for several agents backs up files and folders; choose one agent for the other kinds"))
		return
	}
	g := groupFromForm(r, JobGroup{TargetID: formID(r, "target_id"), Schedule: sched, Retention: retentionFromForm(r),
		Options: optionsFromForm(r, JobOptions{}, canCommands(r))})
	id, err := s.store.CreateJobGroup(r.Context(), g)
	if err != nil {
		redirectErr(w, r, "/jobs", err)
		return
	}
	s.audit(r, "group.create", "#%d %s (%s)", id, g.Name, g.Scope)
	if err := s.store.syncGroups(r.Context()); err != nil {
		s.log.Error("group jobs", "err", err)
	}
	redirectMsg(w, r, fmt.Sprintf("/job-groups/%d", id), "Job created.")
}

func (s *Server) groupFromPath(w http.ResponseWriter, r *http.Request) (JobGroup, bool) {
	id, err := pathID(r)
	if err != nil {
		http.NotFound(w, r)
		return JobGroup{}, false
	}
	g, err := s.store.GetJobGroup(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return g, false
	} else if err != nil {
		s.serverError(w, err)
		return g, false
	}
	return g, true
}

func (s *Server) handleGroup(w http.ResponseWriter, r *http.Request, user string) {
	g, ok := s.groupFromPath(w, r)
	if !ok {
		return
	}
	rows, err := s.store.groupRows(r.Context(), g)
	if err != nil {
		s.serverError(w, err)
		return
	}
	failed, ok2 := 0, 0
	for _, row := range rows {
		if row.Job == nil || row.Job.LastStatus == nil {
			continue
		}
		switch *row.Job.LastStatus {
		case "failed":
			failed++
		case "success", "warning":
			ok2++
		}
	}
	s.render(w, r, "jobgroup", pageData{Title: g.Name, Nav: "jobs", User: user,
		Data: map[string]any{"Group": g, "Rows": rows, "Failed": failed, "OK": ok2}})
}

func (s *Server) handleGroupEditPage(w http.ResponseWriter, r *http.Request, user string) {
	g, ok := s.groupFromPath(w, r)
	if !ok {
		return
	}
	agents, err := s.store.ListAgents(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, r, "jobgroupedit", pageData{Title: g.Name, Nav: "jobs", User: user,
		Data: map[string]any{"Group": g, "Agents": agents, "Form": s.jobForm(r, g.member(0), true, nil)}})
}

func (s *Server) handleGroupEdit(w http.ResponseWriter, r *http.Request, _ string) {
	old, ok := s.groupFromPath(w, r)
	if !ok {
		return
	}
	back := fmt.Sprintf("/job-groups/%d", old.ID)
	r.ParseForm()
	sched, err := scheduleFromForm(r)
	if err != nil {
		redirectErr(w, r, back+"/edit", err)
		return
	}
	g := groupFromForm(r, old)
	g.Schedule, g.Retention = sched, retentionFromForm(r)
	g.Options = optionsFromForm(r, old.Options, canCommands(r))
	if err := g.check(); err != nil {
		redirectErr(w, r, back+"/edit", err)
		return
	}
	// With four-eyes approval, an edit that stops backing up agents or
	// shortens the retention waits for a second user as a whole.
	if s.store.fourEyes(r.Context()) && (shorterRetention(old.Retention, g.Retention) || s.store.groupDrops(r.Context(), old, g)) {
		if s.needsApproval(w, r, back, "group.update", old.ID, old.Name, g) {
			return
		}
	}
	if err := s.store.UpdateJobGroup(r.Context(), g); err != nil {
		redirectErr(w, r, back+"/edit", err)
		return
	}
	s.audit(r, "group.update", "#%d %s (%s)", g.ID, g.Name, g.Scope)
	if err := s.store.syncGroups(r.Context()); err != nil {
		s.log.Error("group jobs", "err", err)
	}
	redirectMsg(w, r, back, "Job saved.")
}

// groupDrops reports whether the new settings leave out an agent that the
// group backs up now.
func (s *Store) groupDrops(ctx context.Context, old, cur JobGroup) bool {
	agents, err := s.ListAgents(ctx)
	if err != nil {
		return true
	}
	for _, a := range agents {
		if old.Matches(a) && !cur.Matches(a) {
			return true
		}
	}
	return false
}

func (s *Server) handleGroupRun(w http.ResponseWriter, r *http.Request, _ string) {
	g, ok := s.groupFromPath(w, r)
	if !ok {
		return
	}
	back := fmt.Sprintf("/job-groups/%d", g.ID)
	members, err := s.store.groupMembers(r.Context(), g.ID)
	if err != nil {
		redirectErr(w, r, back, err)
		return
	}
	n := 0
	for _, m := range members {
		if _, err := s.store.QueueBackup(r.Context(), m.ID, "manual"); err == nil {
			n++
		}
	}
	redirectMsg(w, r, back, fmt.Sprintf("Backups queued for %d agents.", n))
}

func (s *Server) handleGroupEnable(enabled bool) func(http.ResponseWriter, *http.Request, string) {
	return func(w http.ResponseWriter, r *http.Request, _ string) {
		g, ok := s.groupFromPath(w, r)
		if !ok {
			return
		}
		back := fmt.Sprintf("/job-groups/%d", g.ID)
		if !enabled && s.needsApproval(w, r, back, "group.disable", g.ID, g.Name, nil) {
			return
		}
		if err := s.store.SetJobGroupEnabled(r.Context(), g.ID, enabled); err != nil {
			redirectErr(w, r, back, err)
			return
		}
		s.audit(r, map[bool]string{true: "group.enable", false: "group.disable"}[enabled], "group job %d %s", g.ID, g.Name)
		redirectMsg(w, r, back, map[bool]string{true: "Job enabled.", false: "Job disabled."}[enabled])
	}
}

func (s *Server) handleGroupDelete(w http.ResponseWriter, r *http.Request, _ string) {
	g, ok := s.groupFromPath(w, r)
	if !ok {
		return
	}
	if s.needsApproval(w, r, fmt.Sprintf("/job-groups/%d", g.ID), "group.delete", g.ID, g.Name, nil) {
		return
	}
	if err := s.store.DeleteJobGroup(r.Context(), g.ID); err != nil {
		redirectErr(w, r, "/jobs", err)
		return
	}
	s.audit(r, "group.delete", "group job %d %s", g.ID, g.Name)
	redirectMsg(w, r, "/jobs", "Job deleted. Existing backups remain on the storage target.")
}

// memberOfGroup sends edits and deletion of a member job to its group.
func (s *Server) memberOfGroup(w http.ResponseWriter, r *http.Request, j Job) bool {
	if j.GroupID == nil {
		return false
	}
	redirectErr(w, r, fmt.Sprintf("/job-groups/%d", *j.GroupID), fmt.Errorf("%s is part of the job for several agents %q: change or delete it here", j.Hostname, deref(j.GroupName)))
	return true
}

// sameJSON compares two values as stored (nil and empty lists alike).
func sameJSON(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(x) == string(y)
}

// groupScopeTitle names the agents a group backs up (English, translated
// by the templates).
func groupScopeTitle(scope string) string {
	switch scope {
	case GroupAll:
		return "All agents"
	case GroupWindows:
		return "All Windows agents"
	case GroupLinux:
		return "All Linux agents"
	case GroupMacOS:
		return "All macOS agents"
	}
	return "Chosen agents"
}

// groupCounts counts the member jobs per group.
func groupCounts(jobs []Job) map[int64]int {
	n := map[int64]int{}
	for _, j := range jobs {
		if j.GroupID != nil {
			n[*j.GroupID]++
		}
	}
	return n
}

// Scopes lists the choices of agents, for the edit form.
func (JobGroup) Scopes() []string {
	return []string{GroupAll, GroupWindows, GroupLinux, GroupMacOS, GroupAgents}
}
