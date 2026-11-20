package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/leftover"
)

// Leftovers (S-3, 0.56): what backups created on a hypervisor (snapshots,
// checkpoints, temporary disk copies, backup locks) and could not remove.
// Agents report their ledger (leftover); the VMware inventory shows BackupZit
// snapshots on its own, also those of older agents. The dashboard lists them
// with Remove now, and each is notified once.

// inventoryLeftoverAge is how old a BackupZit snapshot in the VMware
// inventory must be, while no VM operation of the connection runs, before
// the dashboard lists it: a backup removes its snapshot within minutes of
// reading the disks.
const inventoryLeftoverAge = 6 * time.Hour

// Leftover is one item shown on the dashboard.
type Leftover struct {
	// Key identifies it for the notifications.
	Key     string
	AgentID int64
	Agent   string
	// HostID and Host: the VMware connection (0 and "" for Proxmox VE and
	// Hyper-V, whose agents clean up on their own host).
	HostID int64
	Host   string
	Item   leftover.Item
	// Found: seen in the VMware inventory, not reported by an agent.
	Found bool
}

// minLeftoverAgent is the first agent version with the ledger and the
// clean-up run.
var minLeftoverAgent = [3]int{0, 56, 0}

// saveLeftovers stores the list an agent reported (nil: unchanged).
func (s *Store) saveLeftovers(ctx context.Context, agentID int64, raw json.RawMessage) error {
	if raw == nil {
		return nil
	}
	var items []leftover.Item
	if len(raw) > 1<<20 || json.Unmarshal(raw, &items) != nil {
		return nil // not stored: an old list is better than a broken one
	}
	if len(items) > 200 {
		items = items[len(items)-200:]
	}
	for i := range items {
		it := &items[i]
		it.Platform, it.Kind, it.Host, it.VM = clip(it.Platform, 16), clip(it.Kind, 16), clip(it.Host, 255), clip(it.VM, 64)
		it.VMName, it.Name, it.Detail, it.Error = clip(it.VMName, 255), clip(it.Name, 512), clip(it.Detail, 512), clip(it.Error, 500)
	}
	b, _ := json.Marshal(items)
	_, err := s.db.Exec(ctx, `UPDATE agents SET leftovers=$2, leftovers_at=now() WHERE id=$1`, agentID, b)
	return err
}

// vmwareBusyHosts are the VMware connections with a VM operation running.
func (s *Store) vmwareBusyHosts(ctx context.Context) map[int64]bool {
	busy := map[int64]bool{}
	rows, err := s.db.Query(ctx, `SELECT DISTINCT vmware_host_id FROM runs WHERE status IN ('running','queued') AND vmware_host_id IS NOT NULL
		AND kind IN ('vm-backup','vm-restore','vm-instant','vm-instant-finish','vm-instant-discard','leftover-cleanup')`)
	if err != nil {
		return busy
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			busy[id] = true
		}
	}
	return busy
}

// listLeftovers lists what agents reported and what the VMware inventory
// shows, oldest first.
func (s *Store) listLeftovers(ctx context.Context, now time.Time) []Leftover {
	hosts, _ := s.ListVMwareHosts(ctx)
	byAddr := map[string]VMwareHost{}
	for _, h := range hosts {
		byAddr[strings.ToLower(h.Address)] = h
	}
	var out []Leftover
	seen := map[string]bool{}
	rows, err := s.db.Query(ctx, `SELECT id, COALESCE(NULLIF(display_name,''), hostname), leftovers FROM agents
		WHERE removed_at IS NULL AND leftovers IS NOT NULL AND jsonb_array_length(leftovers) > 0`)
	if err == nil {
		type row struct {
			id    int64
			name  string
			items []byte
		}
		list, _ := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
			var x row
			return x, r.Scan(&x.id, &x.name, &x.items)
		})
		for _, x := range list {
			var items []leftover.Item
			if json.Unmarshal(x.items, &items) != nil {
				continue
			}
			for _, it := range items {
				l := Leftover{Key: fmt.Sprintf("a%d|%s", x.id, it.Key()), AgentID: x.id, Agent: x.name, Item: it}
				if it.Platform == leftover.VMware {
					if h, ok := byAddr[strings.ToLower(it.Host)]; ok {
						l.HostID, l.Host = h.ID, h.Name
					} else {
						l.Host = it.Host
					}
					seen[strings.ToLower(it.Host+"|"+it.VM+"|"+it.Name)] = true
				}
				out = append(out, l)
			}
		}
	}
	busy := s.vmwareBusyHosts(ctx)
	for _, h := range hosts {
		inv := h.Inv()
		if inv == nil || busy[h.ID] || h.ProxyAgentID == nil {
			continue
		}
		for _, g := range inv.Guests {
			for _, sn := range g.BackupSnapshots {
				if now.Sub(sn.Created) < inventoryLeftoverAge || seen[strings.ToLower(h.Address+"|"+g.ID+"|"+sn.Name)] {
					continue
				}
				it := leftover.Item{Platform: leftover.VMware, Kind: leftover.KindSnapshot, Host: h.Address, VM: g.ID, VMName: g.Name, Name: sn.Name, Created: sn.Created}
				out = append(out, Leftover{Key: fmt.Sprintf("h%d|%s|%s", h.ID, g.ID, sn.Name), AgentID: *h.ProxyAgentID, HostID: h.ID, Host: h.Name, Item: it, Found: true})
			}
		}
	}
	sort.SliceStable(out, func(i, k int) bool { return out[i].Item.Created.Before(out[k].Item.Created) })
	return out
}

// leftoverText describes a leftover for the dashboard and notifications.
func leftoverText(l *language, x Leftover, now time.Time) string {
	what := map[string]string{
		leftover.KindSnapshot:   l.T("backup snapshot %s", x.Item.Name),
		leftover.KindCheckpoint: l.T("backup checkpoint %s", x.Item.Name),
		leftover.KindDiskCopy:   l.T("temporary disk copy %s", x.Item.Name),
		leftover.KindLock:       l.T("backup lock"),
	}[x.Item.Kind]
	if what == "" {
		what = x.Item.Kind + " " + x.Item.Name
	}
	where := x.Agent
	if x.Host != "" {
		where = x.Host
	}
	msg := l.T("%s left on %s for %s", what, where, humanAge(l, now.Sub(x.Item.Created)))
	switch {
	case x.Found:
		msg += "; " + l.T("found in the VMware inventory while no backup of the connection runs")
	case x.Item.Error != "":
		msg += "; " + l.T("removing it failed %d times: %s", x.Item.Tries, x.Item.Error)
	case x.Item.Pending:
		msg += "; " + l.T("Hyper-V may still add it under its default name; the agent removes it when it appears")
	case x.Item.Platform == leftover.VMware:
		msg += "; " + l.T("left by an interrupted backup; the proxy removes it at its next backup of the connection")
	default:
		msg += "; " + l.T("left by an interrupted backup; the agent removes it within 15 minutes")
	}
	return msg
}

// leftoverAction is the Remove now form of a leftover ("" when the agent
// cannot do it: too old, offline).
func leftoverAction(x Leftover, agents map[int64]Agent) string {
	a, ok := agents[x.AgentID]
	if !ok || !a.Online() {
		return ""
	}
	if v, ok := parseVersion(a.Version); !ok || versionLess(v, minLeftoverAgent) {
		return ""
	}
	q := url.Values{"agent": {strconv.FormatInt(x.AgentID, 10)}}
	if x.HostID != 0 {
		q.Set("host", strconv.FormatInt(x.HostID, 10))
	}
	if x.Found {
		q.Set("vm", x.Item.VM)
		q.Set("name", x.Item.Name)
	}
	return "/leftovers/clean?" + q.Encode()
}

// leftoverAttention adds the leftovers to the dashboard's Attention list.
func (s *Server) leftoverAttention(ctx context.Context, now time.Time, l *language, agents []Agent) []Attention {
	byID := map[int64]Agent{}
	for _, a := range agents {
		byID[a.ID] = a
	}
	var out []Attention
	for _, x := range s.store.listLeftovers(ctx, now) {
		title := x.Item.VMName
		if title == "" {
			title = l.T("VM %s", x.Item.VM)
		}
		sev := "warning"
		if x.Item.Error != "" || x.Found || now.Sub(x.Item.Created) > 24*time.Hour {
			sev = "failed" // the VM's disks keep growing while it stays
		}
		out = append(out, Attention{Severity: sev, Title: title, Detail: leftoverText(l, x, now), Link: "/agents", Action: leftoverAction(x, byID)})
	}
	return out
}

// queueLeftoverCleanup queues a clean-up run on the agent; with a VMware
// connection it carries the host's credentials and the named snapshots.
func (s *Store) queueLeftoverCleanup(ctx context.Context, agentID int64, hostID *int64, snaps []string, trigger string) (int64, error) {
	if snaps == nil {
		snaps = []string{} // paths is NOT NULL
	}
	var id int64
	err := s.db.QueryRow(ctx, `INSERT INTO runs(agent_id, kind, trigger, repo_url, notified, chat_notified, vmware_host_id, paths)
		SELECT $1,'leftover-cleanup',$4,'',true,true,$2,$3
		WHERE NOT EXISTS (SELECT 1 FROM runs WHERE agent_id=$1 AND kind='leftover-cleanup' AND status IN ('queued','running') AND vmware_host_id IS NOT DISTINCT FROM $2)
		RETURNING id`, agentID, hostID, snaps, trigger).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errors.New("a clean-up of this agent is already queued")
	}
	return id, err
}

// cleanupRun fills in a clean-up run for the agent.
func (s *Server) cleanupRun(ctx context.Context, run *Run) (*api.Run, error) {
	ar := &api.Run{ID: run.ID, Kind: run.Kind}
	for _, p := range run.Paths {
		if vm, name, ok := strings.Cut(p, "|"); ok {
			ar.Cleanup = append(ar.Cleanup, leftover.Item{Kind: leftover.KindSnapshot, VM: vm, Name: name})
		}
	}
	return ar, s.addVMware(ctx, run, ar)
}

// handleLeftoverClean serves POST /leftovers/clean (Remove now on the
// dashboard): agent, and for VMware host; vm and name for a snapshot found
// in the inventory.
func (s *Server) handleLeftoverClean(w http.ResponseWriter, r *http.Request, _ string) {
	l := requestLanguage(r)
	agentID, _ := strconv.ParseInt(r.FormValue("agent"), 10, 64)
	a, err := s.store.GetAgent(r.Context(), agentID)
	if err != nil || a.RemovedAt != nil {
		redirectMsg(w, r, "/", l.T("Agent not found."))
		return
	}
	if v, ok := parseVersion(a.Version); !ok || versionLess(v, minLeftoverAgent) {
		redirectMsg(w, r, "/", l.T("Update the agent on %s to %s or newer to remove leftovers from the console.", a.Name(), "0.56.0"))
		return
	}
	var hostID *int64
	label := a.Name()
	if hid, err := strconv.ParseInt(r.FormValue("host"), 10, 64); err == nil && hid > 0 {
		h, err := s.store.GetVMwareHost(r.Context(), hid)
		if err != nil {
			redirectMsg(w, r, "/", l.T("VMware host not found."))
			return
		}
		ok := h.ProxyAgentID != nil && *h.ProxyAgentID == agentID
		for _, p := range h.ExtraProxies {
			ok = ok || p == agentID
		}
		if !ok {
			redirectMsg(w, r, "/", l.T("%s is not a proxy of %s.", a.Name(), h.Name))
			return
		}
		hostID, label = &h.ID, h.Name
	}
	var snaps []string
	if name := r.FormValue("name"); name != "" {
		vm := r.FormValue("vm")
		if hostID == nil || !strings.HasPrefix(name, "backupzit_") || strings.ContainsAny(name+vm, "|\n") || vm == "" {
			redirectMsg(w, r, "/", l.T("Only BackupZit snapshots can be removed here."))
			return
		}
		snaps = []string{vm + "|" + name}
	}
	id, err := s.store.queueLeftoverCleanup(r.Context(), agentID, hostID, snaps, "manual")
	if err != nil {
		redirectMsg(w, r, "/", err.Error())
		return
	}
	s.audit(r, "leftover.clean", "%s: clean-up run %d (%s)", label, id, strings.Join(snaps, ", "))
	redirectMsg(w, r, fmt.Sprintf("/runs/%d", id), l.T("The agent removes the leftovers it can now; the run shows what was removed."))
}

// autoLeftoverCleanup queues a clean-up on VMware proxies that report a
// leftover never tried yet (left by an agent crash or a power loss): the
// agent cannot remove those on its own, it needs the host's credentials. A
// leftover whose removal failed waits for Remove now, so a stuck one does
// not queue a run every few minutes.
func (s *Store) autoLeftoverCleanup(ctx context.Context, now time.Time) {
	busy := s.vmwareBusyHosts(ctx)
	done := map[string]bool{}
	for _, x := range s.listLeftovers(ctx, now) {
		if x.Found || x.HostID == 0 || x.Item.Tries > 0 || busy[x.HostID] {
			continue
		}
		k := fmt.Sprintf("%d/%d", x.AgentID, x.HostID)
		if done[k] {
			continue
		}
		done[k] = true
		var recent bool
		_ = s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runs WHERE agent_id=$1 AND vmware_host_id=$2 AND kind='leftover-cleanup' AND queued_at > now() - interval '1 hour')`,
			x.AgentID, x.HostID).Scan(&recent)
		if recent {
			continue
		}
		a, err := s.GetAgent(ctx, x.AgentID)
		if err != nil || !a.Online() {
			continue
		}
		if v, ok := parseVersion(a.Version); !ok || versionLess(v, minLeftoverAgent) {
			continue
		}
		hid := x.HostID
		_, _ = s.queueLeftoverCleanup(ctx, x.AgentID, &hid, nil, "auto")
	}
}

// leftoverAlerts notifies each leftover once (email on warnings, chat
// channels on failures and warnings), like the RPO alerts.
func (n *Notifier) leftoverAlerts(ctx context.Context, l *language, e EmailSettings, chat ChatSettings, now time.Time) {
	sent := map[string]bool{}
	_ = n.store.GetSetting(ctx, settingLeftoverAlerts, &sent)
	next := map[string]bool{}
	for _, x := range n.store.listLeftovers(ctx, now) {
		// Only what stays: a crash leftover the agent removes in its next
		// pass is not worth a message.
		if !x.Found && x.Item.Error == "" && now.Sub(x.Item.Created) < time.Hour {
			continue
		}
		next[x.Key] = true
		if sent[x.Key] {
			continue
		}
		title := x.Item.VMName
		if title == "" {
			title = x.Item.VM
		}
		subject := "[BackupZit] " + l.T("Leftover on VM %s", title)
		text := leftoverText(l, x, now)
		body := text + "\n\n" + l.T("While it stays, the VM's disks grow (snapshot, checkpoint) or the VM cannot start (lock). Remove now on the dashboard asks the agent to remove it.") + "\n"
		if u := n.publicURL(); u != "" {
			body += "\n" + l.T("Details:") + " " + u + "/\n"
		}
		if e.Enabled && (e.OnWarning || e.OnFailure) && e.Host != "" && len(e.To) > 0 {
			if err := n.send(ctx, e, subject, body); err != nil {
				n.log.Warn("send leftover notification", "key", x.Key, "err", err)
				delete(next, x.Key) // again on the next pass
				continue
			}
		}
		for _, c := range chat.Channels {
			if !c.OnFailure && !c.OnWarning {
				continue
			}
			if err := n.sendChatText(ctx, l, c, strings.TrimPrefix(subject, "[BackupZit] "), text, "/", map[string]any{
				"event": "leftover", "vm": title, "kind": x.Item.Kind, "name": x.Item.Name, "agent": x.Agent, "host": x.Host, "since": x.Item.Created}); err != nil {
				n.log.Warn("send leftover chat notification", "key", x.Key, "channel", c.Name, "err", err)
			}
		}
		n.log.Warn("leftover of a backup on a hypervisor", "vm", title, "item", x.Item.Kind+" "+x.Item.Name)
	}
	if b1, _ := json.Marshal(sent); string(b1) != mustJSON(next) {
		_ = n.store.SetSetting(ctx, settingLeftoverAlerts, next)
	}
}
