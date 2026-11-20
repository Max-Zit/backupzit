package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/pve"
	"github.com/max-zit/backupzit/internal/vmware"
)

// A VMware connection (vCenter) can have further proxy agents besides its
// own (0.55, V-1). A job's backup is then split: each VM goes to a proxy
// whose VM runs on the same ESXi host, so it reads the disks through
// hot-add, and every other VM to the connection's proxy, which copies them
// on the datastore if it cannot reach them. Each part is a run of its own
// on its proxy; part tags its snapshots (retention keeps N backups per
// part) and batch joins the parts of one start (chains follow once).

// vmwarePart is the share of one proxy in a job's backup.
type vmwarePart struct {
	agentID         int64
	paths, excludes []string
	primary         bool
}

// partTag is the snapshot tag of a part.
func partTag(agentID int64) string { return fmt.Sprintf("proxy:%d", agentID) }

// vmwareParts splits the VMs of a VMware job over the connection's proxies
// as the inventory places them. nil: the connection has no further proxy
// (or no inventory yet), and one run on its proxy backs up all, as before.
func (s *Store) vmwareParts(ctx context.Context, h VMwareHost, j Job) []vmwarePart {
	return splitVMware(h, j, func(id int64) string { return s.agentMachineUUID(ctx, id) })
}

// splitVMware is vmwareParts with the agents' machine UUIDs from machineUUID.
func splitVMware(h VMwareHost, j Job, machineUUID func(int64) string) []vmwarePart {
	if len(h.ExtraProxies) == 0 || h.ProxyAgentID == nil {
		return nil
	}
	inv := h.Inv()
	if inv == nil {
		return nil
	}
	primary := *h.ProxyAgentID
	// The ESXi host of each proxy: its own VM in the inventory, found by the
	// BIOS UUID the agent reports. Only VMs on a known ESXi host count (Node
	// is the vCenter itself when vCenter did not say).
	onHost := func(g pve.Guest) bool { return g.Node != "" && slices.Contains(inv.Hosts, g.Node) }
	hostProxy := map[string]int64{}
	// The connection's proxy keeps the VMs of its own host.
	for _, id := range append([]int64{primary}, h.ExtraProxies...) {
		mu := machineUUID(id)
		if mu == "" {
			continue
		}
		for _, g := range inv.Guests {
			if onHost(g) && vmware.SameMachine(mu, g.BIOSUUID) {
				if _, taken := hostProxy[g.Node]; !taken {
					hostProxy[g.Node] = id
				}
				break
			}
		}
	}
	all := len(j.Paths) == 1 && j.Paths[0] == "*"
	want, excl := map[int]bool{}, map[int]bool{}
	for _, p := range j.Paths {
		if n, err := strconv.Atoi(p); err == nil {
			want[n] = true
		}
	}
	for _, p := range j.Excludes {
		if n, err := strconv.Atoi(p); err == nil {
			excl[n] = true
		}
	}
	// The same choice the agent makes (vmware.Backup): by VM number, or by
	// the BIOS number of VMs from before vCenter took the host over.
	selected := func(g pve.Guest) bool {
		if all {
			return !excl[g.VMID] && !(g.FormerVMID != 0 && excl[g.FormerVMID])
		}
		return want[g.VMID] || (g.FormerVMID != 0 && want[g.FormerVMID])
	}
	assigned := map[int64][]pve.Guest{}
	moved := map[string]bool{} // VM numbers another proxy takes
	left := 0
	for _, g := range inv.Guests {
		if !selected(g) {
			continue
		}
		if id, ok := hostProxy[g.Node]; ok && onHost(g) && id != primary {
			assigned[id] = append(assigned[id], g)
			moved[strconv.Itoa(g.VMID)] = true
			if g.FormerVMID != 0 {
				moved[strconv.Itoa(g.FormerVMID)] = true
			}
			continue
		}
		left++
	}
	var parts []vmwarePart
	// The connection's proxy keeps the job's own choice minus the moved VMs:
	// a VM added since the inventory is backed up there, not missed.
	p := vmwarePart{agentID: primary, primary: true}
	if all {
		p.paths = []string{"*"}
		p.excludes = append([]string(nil), j.Excludes...)
		for n := range moved {
			p.excludes = append(p.excludes, n)
		}
		sort.Strings(p.excludes)
	} else {
		for _, n := range j.Paths {
			if !moved[n] {
				p.paths = append(p.paths, n)
			}
		}
	}
	if left > 0 || (!all && len(p.paths) > 0) {
		parts = append(parts, p)
	}
	ids := make([]int64, 0, len(assigned))
	for id := range assigned {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
	for _, id := range ids {
		var ps []string
		for _, g := range assigned[id] {
			ps = append(ps, strconv.Itoa(g.VMID))
		}
		parts = append(parts, vmwarePart{agentID: id, paths: ps})
	}
	if len(parts) == 0 {
		return nil
	}
	return parts
}

// queueVMwareParts queues one run per part; it fails with ErrRunActive
// while a backup of the job is queued or running.
func (s *Store) queueVMwareParts(ctx context.Context, j Job, repoURL string, targetID int64, trigger string, parts []vmwarePart) (int64, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var first int64
	for i, p := range parts {
		excludes := p.excludes
		if excludes == nil {
			excludes = []string{}
		}
		var id int64
		if i == 0 {
			err = tx.QueryRow(ctx, `INSERT INTO runs(agent_id, job_id, kind, trigger, repo_url, target_id, paths, excludes, vmware_host_id, part)
				SELECT $1,$2,'vm-backup',$3,$4,$5,$6,$7,$8,$9
				WHERE NOT EXISTS (SELECT 1 FROM runs WHERE job_id=$2 AND kind IN ('backup','image-backup','vm-backup','system-backup','sql-backup','sql-log') AND status IN ('queued','running'))
				RETURNING id`, p.agentID, j.ID, trigger, repoURL, targetID, p.paths, excludes, j.VMwareHostID, partTag(p.agentID)).Scan(&id)
			if errors.Is(err, pgx.ErrNoRows) {
				return 0, ErrRunActive
			}
			first = id
		} else {
			err = tx.QueryRow(ctx, `INSERT INTO runs(agent_id, job_id, kind, trigger, repo_url, target_id, paths, excludes, vmware_host_id, part, batch)
				VALUES($1,$2,'vm-backup',$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`,
				p.agentID, j.ID, trigger, repoURL, targetID, p.paths, excludes, j.VMwareHostID, partTag(p.agentID), first).Scan(&id)
		}
		if err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET batch=$1 WHERE id=$1`, first); err != nil {
		return 0, err
	}
	return first, tx.Commit(ctx)
}

// batchOK is an SQL condition on the runs row t: a part of a split backup
// counts as a good backup only once every part of its start succeeded (a
// failed proxy must not hide behind the others: RPO, the dashboard).
func batchOK(t string) string {
	return "(" + t + ".batch IS NULL OR NOT EXISTS (SELECT 1 FROM runs o WHERE o.batch=" + t + ".batch AND o.status NOT IN ('success','warning')))"
}

// lastRunSQL is the job's newest backup run (lr: status, finished_at,
// batch); lastStatusSQL and lastFinishedSQL read a split start as a whole:
// running while a part runs, failed when a part failed, finished when the
// last part finished.
const lastRunSQL = `(SELECT r.status, r.finished_at, r.batch FROM runs r WHERE r.job_id=j.id AND r.kind IN ('backup','image-backup','vm-backup','system-backup','copy','sql-backup','sql-log') ORDER BY r.queued_at DESC, r.id DESC LIMIT 1) lr`

const lastStatusSQL = `(SELECT CASE WHEN lr.batch IS NULL THEN lr.status ELSE (SELECT CASE WHEN bool_or(o.status='running') THEN 'running' WHEN bool_or(o.status='queued') THEN 'queued' WHEN bool_or(o.status='failed') THEN 'failed' WHEN bool_or(o.status='cancelled') THEN 'cancelled' WHEN bool_or(o.status='warning') THEN 'warning' ELSE 'success' END FROM runs o WHERE o.batch=lr.batch) END FROM ` + lastRunSQL + `)`

const lastFinishedSQL = `(SELECT CASE WHEN lr.batch IS NULL THEN lr.finished_at ELSE (SELECT CASE WHEN bool_and(o.finished_at IS NOT NULL) THEN max(o.finished_at) END FROM runs o WHERE o.batch=lr.batch) END FROM ` + lastRunSQL + `)`

// minPartsVersion is the first agent that keeps retention per part; an
// older one would count the other proxies' backups as its own and remove
// them.
const minPartsVersion = "0.55.0"

// addPart hands a part of a split VMware backup its tag and, to the
// connection's own proxy, the parts of the other proxies (retention).
func (s *Server) addPart(ctx context.Context, run *Run, ar *api.Run) error {
	if run.Part == "" {
		return nil
	}
	if a, err := s.store.GetAgent(ctx, run.AgentID); err == nil && agentOlder(a, minPartsVersion) {
		return fmt.Errorf("the agent on %s (%s) cannot take part in a backup over several proxies yet; update it to %s or newer (Agents page)", a.Hostname, a.Version, minPartsVersion)
	}
	ar.Part = run.Part
	h, err := s.store.vmwareHostOfRun(ctx, run.ID)
	if err != nil || h == nil || h.ProxyAgentID == nil || *h.ProxyAgentID != run.AgentID {
		return err // a further proxy: only its own part
	}
	ar.PartPrimary = true
	for _, id := range h.ExtraProxies {
		ar.PartOthers = append(ar.PartOthers, partTag(id))
	}
	return nil
}

// proxyChoice is a Linux agent offered as a further proxy of a vCenter
// connection (Agents page): Host is the ESXi host its VM runs on.
type proxyChoice struct {
	ID    int64
	Label string
	Host  string
	On    bool
	Old   bool // older than minPartsVersion
}

// machineUUIDs are the BIOS UUIDs the agents report, by agent.
func (s *Store) machineUUIDs(ctx context.Context) map[int64]string {
	out := map[int64]string{}
	rows, err := s.db.Query(ctx, `SELECT id, machine_uuid FROM agents WHERE removed_at IS NULL AND machine_uuid <> ''`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var u string
		if rows.Scan(&id, &u) == nil {
			out[id] = u
		}
	}
	return out
}

// proxyHost is the ESXi host of the inventory VM with the machine UUID.
func proxyHost(inv *pve.Inventory, machineUUID string) string {
	if inv == nil || machineUUID == "" {
		return ""
	}
	for _, g := range inv.Guests {
		if g.Node != "" && slices.Contains(inv.Hosts, g.Node) && vmware.SameMachine(machineUUID, g.BIOSUUID) {
			return g.Node
		}
	}
	return ""
}

// proxyAgentOK: hot-add proxies are Linux agents (not a recovery system).
func proxyAgentOK(a Agent) bool {
	return strings.Contains(strings.ToLower(a.OS), "linux") && !a.Recovery
}

// proxyChoices are the further proxies offered for each vCenter connection.
func (s *Store) proxyChoices(ctx context.Context, hosts []VMwareHost, agents []Agent) map[int64][]proxyChoice {
	out := map[int64][]proxyChoice{}
	var uuids map[int64]string
	for _, h := range hosts {
		if !h.IsVCenter() {
			continue
		}
		if uuids == nil {
			uuids = s.machineUUIDs(ctx)
		}
		inv := h.Inv()
		for _, a := range agents {
			if !proxyAgentOK(a) || (h.ProxyAgentID != nil && *h.ProxyAgentID == a.ID) {
				continue
			}
			out[h.ID] = append(out[h.ID], proxyChoice{ID: a.ID, Label: a.Label(), Host: proxyHost(inv, uuids[a.ID]),
				On: slices.Contains(h.ExtraProxies, a.ID), Old: agentOlder(a, minPartsVersion)})
		}
	}
	return out
}

// maxExtraProxies bounds the further proxies of a connection.
const maxExtraProxies = 32

// handleVMwareProxies serves POST /vmware/{id}/proxies: the further proxy
// agents of a vCenter connection (each backs up the VMs of its ESXi host).
func (s *Server) handleVMwareProxies(w http.ResponseWriter, r *http.Request, _ string) {
	id, _ := pathID(r)
	h, err := s.store.GetVMwareHost(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !h.IsVCenter() {
		redirectErr(w, r, "/agents#vmware", errors.New("further proxies need a vCenter Server connection"))
		return
	}
	_ = r.ParseForm()
	var ids []int64
	var names []string
	for _, v := range r.PostForm["agent_id"] {
		aid, err := strconv.ParseInt(v, 10, 64)
		if err != nil || aid <= 0 || slices.Contains(ids, aid) || (h.ProxyAgentID != nil && *h.ProxyAgentID == aid) {
			continue
		}
		a, err := s.store.GetAgent(r.Context(), aid)
		if err != nil || a.RemovedAt != nil {
			redirectErr(w, r, "/agents#vmware", errors.New("unknown agent"))
			return
		}
		if !proxyAgentOK(a) {
			redirectErr(w, r, "/agents#vmware", fmt.Errorf("%s is not a Linux agent; a proxy for hot-add runs Linux in a VM of the vCenter", a.Hostname))
			return
		}
		if agentOlder(a, minPartsVersion) {
			redirectErr(w, r, "/agents#vmware", fmt.Errorf("the agent on %s (%s) cannot be a further proxy yet; update it to %s or newer", a.Hostname, a.Version, minPartsVersion))
			return
		}
		ids, names = append(ids, aid), append(names, a.Hostname)
	}
	if len(ids) > maxExtraProxies {
		redirectErr(w, r, "/agents#vmware", fmt.Errorf("at most %d further proxies", maxExtraProxies))
		return
	}
	if err := s.store.setVMwareExtraProxies(r.Context(), id, ids); err != nil {
		s.serverError(w, err)
		return
	}
	l := requestLanguage(r)
	if len(names) == 0 {
		s.audit(r, "vmware.proxies", "%s: none", h.Name)
		redirectMsg(w, r, "/agents#vmware", l.T("%s is backed up through its proxy only.", h.Name))
		return
	}
	s.audit(r, "vmware.proxies", "%s: %s", h.Name, strings.Join(names, ", "))
	redirectMsg(w, r, "/agents#vmware", l.T("%s: further proxies %s. Each backs up the VMs of its ESXi host.", h.Name, strings.Join(names, ", ")))
}

// copyParts are the part tags a copy of the source job runs in: those of
// the proxies of its vCenter connection, its own first (nil: one run).
func (s *Store) copyParts(ctx context.Context, src Job) []string {
	if src.Kind != JobVM || src.VMwareHostID == nil {
		return nil
	}
	h, err := s.GetVMwareHost(ctx, *src.VMwareHostID)
	if err != nil || h.ProxyAgentID == nil || len(h.ExtraProxies) == 0 {
		return nil
	}
	parts := []string{partTag(*h.ProxyAgentID)}
	for _, id := range h.ExtraProxies {
		parts = append(parts, partTag(id))
	}
	return parts
}

// queueCopyParts queues one copy run per part on the copy job's agent; it
// fails with ErrRunActive while a copy of the job is queued or running.
func (s *Store) queueCopyParts(ctx context.Context, j Job, trigger, repo string, targetID, srcTargetID int64, srcRepo string, parts []string) (int64, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var first int64
	for i, part := range parts {
		var id int64
		if i == 0 {
			err = tx.QueryRow(ctx, `INSERT INTO runs(agent_id, job_id, kind, trigger, repo_url, target_id, source_target_id, source_repo_url, part)
				SELECT $1,$2,'copy',$3,$4,$5,$6,$7,$8
				WHERE NOT EXISTS (SELECT 1 FROM runs WHERE job_id=$2 AND kind='copy' AND status IN ('queued','running'))
				RETURNING id`, j.AgentID, j.ID, trigger, repo, targetID, srcTargetID, srcRepo, part).Scan(&id)
			if errors.Is(err, pgx.ErrNoRows) {
				return 0, ErrRunActive
			}
			first = id
		} else {
			err = tx.QueryRow(ctx, `INSERT INTO runs(agent_id, job_id, kind, trigger, repo_url, target_id, source_target_id, source_repo_url, part, batch)
				VALUES($1,$2,'copy',$3,$4,$5,$6,$7,$8,$9) RETURNING id`, j.AgentID, j.ID, trigger, repo, targetID, srcTargetID, srcRepo, part, first).Scan(&id)
		}
		if err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET batch=$1 WHERE id=$1`, first); err != nil {
		return 0, err
	}
	return first, tx.Commit(ctx)
}

// addCopyPart gives a copy run of a split backup its part: the copy of the
// connection's own proxy also takes the backups without a part.
func (s *Server) addCopyPart(ctx context.Context, run *Run, ar *api.Run) error {
	if run.Part == "" || run.JobID == nil {
		return nil
	}
	if a, err := s.store.GetAgent(ctx, run.AgentID); err == nil && agentOlder(a, minPartsVersion) {
		return fmt.Errorf("the agent on %s (%s) cannot copy a backup over several proxies yet; update it to %s or newer (Agents page)", a.Hostname, a.Version, minPartsVersion)
	}
	ar.Part = run.Part
	j, err := s.store.GetJob(ctx, *run.JobID)
	if err != nil || j.SourceJobID == nil {
		return nil
	}
	src, err := s.store.GetJob(ctx, *j.SourceJobID)
	if err != nil {
		return nil
	}
	parts := s.store.copyParts(ctx, src)
	if len(parts) > 0 && parts[0] == run.Part {
		ar.PartPrimary, ar.PartOthers = true, parts[1:]
	}
	return nil
}

// batchCopies tells whether the copies after a split backup may start: all
// its parts finished and at least one has a backup.
func (s *Store) batchCopies(ctx context.Context, batch int64) bool {
	var ok bool
	_ = s.db.QueryRow(ctx, `SELECT NOT bool_or(status IN ('queued','running')) AND bool_or(status IN ('success','warning')) FROM runs WHERE batch=$1`, batch).Scan(&ok)
	return ok
}
