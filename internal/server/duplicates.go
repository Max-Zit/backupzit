package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// dupMaxInstances bounds the instances kept per agent: an agent that sends
// a new instance with every poll cannot grow the console's memory or the
// work done under the lock (MED 1).
const dupMaxInstances = 8

// live drops the instances of an agent that stopped polling (a restarted
// agent or a stopped copy) and returns the others. d.mu is held.
func (d *dupTracker) live(agentID int64, now time.Time) []*agentInstance {
	list := d.inst[agentID][:0]
	for _, x := range d.inst[agentID] {
		if now.Sub(x.last) <= dupWindow {
			list = append(list, x)
		}
	}
	if len(list) == 0 {
		delete(d.inst, agentID)
		return nil
	}
	d.inst[agentID] = list
	return list
}

// seen records a poll. held: the agent's work waits, because two machines
// use the agent (dup; fresh when that is new, to alert once) or because
// this instance is new while an older one polled moments ago (M-9).
// quiet is how long the older instance must stay silent for the new one to
// count as its restart: a little more than the poll interval.
func (d *dupTracker) seen(agentID int64, instance, host, ip string, now time.Time, quiet time.Duration) (held, dup, fresh bool) {
	if instance == "" { // agents before 0.33.4
		return false, false, false
	}
	// The agent sends these; the tracker keeps them (MED 1).
	instance, host, ip = clip(instance, 64), clip(host, 255), clip(ip, 64)
	d.mu.Lock()
	defer d.mu.Unlock()
	list := d.live(agentID, now)
	var me *agentInstance
	for _, x := range list {
		if x.id == instance {
			me = x
		}
	}
	if me == nil {
		me = &agentInstance{id: instance, first: now}
		list = append(list, me)
	}
	me.last, me.host, me.ip = now, host, ip
	if len(list) > dupMaxInstances {
		// Keep the ones that polled last (this one included).
		slices.SortStableFunc(list, func(a, b *agentInstance) int { return b.last.Compare(a.last) })
		clear(list[dupMaxInstances:])
		list = list[:dupMaxInstances]
	}
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
	// A new instance while an older one still polls is a restart (the old
	// one then falls silent) or a copy that just started (it keeps
	// polling, which makes dup above). Until it is clear which, the new
	// instance's empty list of runs must not fail the runs of the older
	// one, nor may it take the older one's work (M-9).
	held = dup
	for _, x := range list {
		if x != me && x.first.Before(me.first) && now.Sub(x.last) <= quiet {
			held = true
		}
	}
	if !dup {
		if _, was := d.since[agentID]; was && len(list) == 1 {
			delete(d.since, agentID)
			delete(d.reported, agentID)
		}
		return held, false, false
	}
	if _, ok := d.since[agentID]; !ok {
		d.since[agentID] = now
	}
	if !d.reported[agentID] {
		d.reported[agentID] = true
		return true, true, true
	}
	return true, true, false
}

// current lists the agents that are used by two machines now.
func (d *dupTracker) current(now time.Time) map[int64]AgentDuplicate {
	d.mu.Lock()
	defer d.mu.Unlock()
	// Agents that stopped polling (removed, or shut down) are forgotten.
	for id := range d.inst {
		if d.live(id, now) == nil {
			delete(d.since, id)
			delete(d.reported, id)
		}
	}
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
// two machines use its enrollment (alerting the administrators once), and
// briefly after a new agent process appeared (see dupTracker.seen).
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
	// A restarted agent is told from a copy once the old process missed a
	// poll; agents poll every 15 seconds while they run something.
	quiet := time.Duration(max(s.PollInterval, 1))*time.Second + 10*time.Second
	held, _, fresh := s.dups.seen(a.ID, instance, host, addr, s.clock(), quiet)
	if fresh {
		m := s.dups.current(s.clock())[a.ID].Machines
		s.log.Warn("two machines use one agent enrollment; its runs are held", "agent", a.Hostname, "machines", m)
		if _, err := s.store.AppendAudit(ctx, "", "agent.duplicate", fmt.Sprintf("agent %s is used by two machines: %v; runs are held", a.Hostname, m), ip); err != nil {
			s.log.Error("audit log", "err", err)
		}
		go s.mailAdmins(context.WithoutCancel(ctx), func(l *language) (string, string) {
			return "[BackupZit] " + l.T("Two machines use one agent: %s", strings.Join(m, ", ")),
				l.T("Two machines are connected to the console as the agent %q right now: %v.", a.Hostname, m) + "\n\n" +
					l.T("This happens when a copy of the machine runs (a VM restored as a new VM, an instant recovery, a started replica or a cloned disk).") + " " +
					l.T("Backups and restores of this agent are held so they cannot run on the wrong machine.") + "\n\n" +
					l.T("Stop the copy, or enroll it as a new machine there: %s (Agents page).", `backupzit-agent enroll --force --code "BZ1-..."`) + " " +
					l.T("The runs continue a few minutes after only one machine is left.") + "\n"
		})
	}
	return held
}

// legacyInstance identifies an agent that sends no instance ID.
func legacyInstance(host string, localIPs []string) string {
	if len(localIPs) == 0 {
		return ""
	}
	if len(localIPs) > 16 {
		localIPs = localIPs[:16] // as the console stores them (TouchAgent)
	}
	ips := append([]string(nil), localIPs...)
	sort.Strings(ips)
	id := "legacy:" + strings.ToLower(clip(host, 255)) + "/" + strings.Join(ips, ",")
	if len(id) > 64 {
		// The tracker keeps 64 bytes of an instance; a longer ID (many
		// addresses) is kept as its hash, which still tells machines apart.
		sum := sha256.Sum256([]byte(id))
		id = "legacy:" + hex.EncodeToString(sum[:16])
	}
	return id
}
