package server

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Two machines can share one enrollment when a copy of an enrolled machine
// runs: a VM restored as a new VM, an instant recovery, a started replica,
// a cloned disk. Both agents then poll as the same agent, and the console
// could hand a backup or a restore to the wrong machine. Every agent
// process sends a random instance ID; an instance that keeps polling after
// a newer one appeared means two live machines (a restart of the agent
// only replaces the instance).

// dupWindow is how long an instance counts as alive after its last poll.
const dupWindow = 3 * time.Minute

type agentInstance struct {
	id          string
	first, last time.Time
	host, ip    string
}

// AgentDuplicate describes an agent whose enrollment is used by two
// machines at the moment.
type AgentDuplicate struct {
	AgentID  int64
	Machines []string // "host (ip)" of each live instance
	Since    time.Time
}

type dupTracker struct {
	mu       sync.Mutex
	inst     map[int64][]*agentInstance
	since    map[int64]time.Time // when the duplicate was first seen
	reported map[int64]bool
}

func newDupTracker() *dupTracker {
	return &dupTracker{inst: map[int64][]*agentInstance{}, since: map[int64]time.Time{}, reported: map[int64]bool{}}
}

// seen records a poll and reports whether the agent is currently used by
// more than one machine, and whether that is new (to alert once).
func (d *dupTracker) seen(agentID int64, instance, host, ip string, now time.Time) (dup, fresh bool) {
	if instance == "" { // agents before 0.33.4
		return false, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	list := d.inst[agentID][:0]
	var me *agentInstance
	for _, x := range d.inst[agentID] {
		if now.Sub(x.last) > dupWindow {
			continue // gone (restarted agent or stopped copy)
		}
		if x.id == instance {
			me = x
		}
		list = append(list, x)
	}
	if me == nil {
		me = &agentInstance{id: instance, first: now}
		list = append(list, me)
	}
	me.last, me.host, me.ip = now, host, ip
	d.inst[agentID] = list
	// An older instance that polled after a newer one started is a second
	// machine; a few seconds of overlap is a restart in progress.
	for _, a := range list {
		for _, b := range list {
			if a != b && a.first.Before(b.first) && a.last.After(b.first.Add(10*time.Second)) {
				dup = true
			}
		}
	}
	if !dup {
		if _, was := d.since[agentID]; was && len(list) == 1 {
			delete(d.since, agentID)
			delete(d.reported, agentID)
		}
		return false, false
	}
	if _, ok := d.since[agentID]; !ok {
		d.since[agentID] = now
	}
	if !d.reported[agentID] {
		d.reported[agentID] = true
		return true, true
	}
	return true, false
}

// current lists the agents that are used by two machines now.
func (d *dupTracker) current(now time.Time) map[int64]AgentDuplicate {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[int64]AgentDuplicate{}
	for id, since := range d.since {
		var m []string
		for _, x := range d.inst[id] {
			if now.Sub(x.last) <= dupWindow {
				m = append(m, fmt.Sprintf("%s (%s)", x.host, x.ip))
			}
		}
		if len(m) < 2 {
			continue
		}
		sort.Strings(m)
		m = slices.Compact(m) // a restarted agent shows the same machine twice
		if len(m) < 2 {
			continue
		}
		out[id] = AgentDuplicate{AgentID: id, Machines: m, Since: since}
	}
	return out
}

// agentDuplicate is called on every poll; it holds the agent's work while
// two machines use its enrollment and alerts the administrators once.
func (s *Server) agentDuplicate(ctx context.Context, a Agent, instance, host string, localIPs []string, ip string) bool {
	if instance == "" {
		// Agents before 0.33.4 send no instance: tell machines apart by
		// their name and own addresses (a copy gets another address; a
		// router in between does not change them).
		instance = legacyInstance(host, localIPs)
	}
	// Machines are shown with their own address (several can share one
	// address on the way, behind NAT).
	addr := ip
	if len(localIPs) > 0 {
		addr = localIPs[0]
	}
	dup, fresh := s.dups.seen(a.ID, instance, host, addr, s.clock())
	if fresh {
		m := s.dups.current(s.clock())[a.ID].Machines
		s.log.Warn("two machines use one agent enrollment; its runs are held", "agent", a.Hostname, "machines", m)
		if _, err := s.store.AppendAudit(ctx, "", "agent.duplicate", fmt.Sprintf("agent %s is used by two machines: %v; runs are held", a.Hostname, m), ip); err != nil {
			s.log.Error("audit log", "err", err)
		}
		go s.mailAdmins(context.WithoutCancel(ctx), "[BackupZit] Two machines use one agent: "+strings.Join(m, ", "),
			fmt.Sprintf("Two machines are connected to the console as the agent %q right now: %v.\n\n"+
				"This happens when a copy of the machine runs (a VM restored as a new VM, an instant recovery, a started replica or a cloned disk). "+
				"Backups and restores of this agent are held so they cannot run on the wrong machine.\n\n"+
				"Stop the copy, or enroll it as a new machine there: backupzit-agent enroll --force --code \"BZ1-...\" (Agents page). "+
				"The runs continue a few minutes after only one machine is left.\n", a.Hostname, m))
	}
	return dup
}

// legacyInstance identifies an agent that sends no instance ID.
func legacyInstance(host string, localIPs []string) string {
	if len(localIPs) == 0 {
		return ""
	}
	ips := append([]string(nil), localIPs...)
	sort.Strings(ips)
	return "legacy:" + strings.ToLower(host) + "/" + strings.Join(ips, ",")
}
