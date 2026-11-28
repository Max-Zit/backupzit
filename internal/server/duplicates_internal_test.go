package server

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const testQuiet = 40 * time.Second

func TestDupTracker(t *testing.T) {
	d := newDupTracker()
	t0 := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	poll := func(inst, host string, at time.Duration) (bool, bool) {
		_, dup, fresh := d.seen(7, inst, host, "10.0.0.1", t0.Add(at), testQuiet)
		return dup, fresh
	}
	// One machine polling, then its agent restarts (new instance): no alarm.
	for i := 0; i < 5; i++ {
		if dup, _ := poll("a", "web1", time.Duration(i)*30*time.Second); dup {
			t.Fatal("single instance reported as duplicate")
		}
	}
	for i := 5; i < 10; i++ {
		if dup, _ := poll("b", "web1", time.Duration(i)*30*time.Second+5*time.Second); dup {
			t.Fatal("agent restart reported as duplicate")
		}
	}
	// A copy starts (instance c) while b keeps polling.
	if dup, _ := poll("c", "web1-copy", 310*time.Second); dup {
		t.Fatal("a new instance alone is not yet a duplicate")
	}
	dup, fresh := poll("b", "web1", 330*time.Second)
	if !dup || !fresh {
		t.Fatalf("older instance polling after a newer one: dup=%v fresh=%v", dup, fresh)
	}
	if dup, fresh := poll("c", "web1-copy", 340*time.Second); !dup || fresh {
		t.Fatalf("duplicate must stay and be alerted once: dup=%v fresh=%v", dup, fresh)
	}
	if m := d.current(t0.Add(340 * time.Second))[7].Machines; len(m) != 2 {
		t.Fatalf("machines = %v", m)
	}
	// The copy is stopped: after the window only b is left and runs resume.
	var last bool
	for i := 0; i < 10; i++ {
		last, _ = poll("b", "web1", 340*time.Second+time.Duration(i+1)*30*time.Second)
	}
	if last {
		t.Fatal("duplicate not cleared after the copy stopped")
	}
	if len(d.current(t0.Add(time.Hour))) != 0 {
		t.Fatal("cleared duplicate still listed")
	}
	// Agents before 0.33.4 send no instance.
	if held, dup, _ := d.seen(8, "", "old", "10.0.0.2", t0, testQuiet); dup || held {
		t.Fatal("agent without instance reported")
	}
}

// M-9: the first polls of a new instance are held while the older one
// polled moments ago, so a copy that just started cannot fail the
// original's runs with its empty list or take its work; a restart is held
// only until the old process missed a poll.
func TestDupTrackerNewInstanceHeld(t *testing.T) {
	d := newDupTracker()
	t0 := time.Date(2026, 11, 28, 9, 0, 0, 0, time.UTC)
	held := func(inst string, at time.Duration) bool {
		h, _, _ := d.seen(5, inst, "db1", "10.0.0.7", t0.Add(at), testQuiet)
		return h
	}
	// The original runs a backup: it polls every 15 seconds.
	for i := 0; i < 4; i++ {
		if held("orig", time.Duration(i)*15*time.Second) {
			t.Fatal("a single instance is held")
		}
	}
	// A copy's first poll, 5 seconds after the original's last one.
	if !held("copy", 50*time.Second) {
		t.Fatal("the first poll of a copy is not held (it would fail the original's runs)")
	}
	if held("orig", 60*time.Second) {
		t.Fatal("the original is held before it is clear that a copy runs")
	}
	if !held("orig", 75*time.Second) || !held("copy", 80*time.Second) {
		t.Fatal("a running copy and its original are not held")
	}

	// A restart: the old process stops, the new one is held briefly.
	d2 := newDupTracker()
	at := func(inst string, sec int) bool {
		h, dup, _ := d2.seen(6, inst, "web2", "10.0.0.8", t0.Add(time.Duration(sec)*time.Second), testQuiet)
		if dup {
			t.Fatalf("restart reported as duplicate at %d s", sec)
		}
		return h
	}
	at("old", 0)
	at("old", 30)
	if !at("new", 35) {
		t.Fatal("new instance not held right after the old one polled")
	}
	if at("new", 75) {
		t.Fatal("restart still held after the old process stayed silent")
	}
}

// MED 1: what an agent sends is clipped, and an agent that sends a new
// instance with every poll keeps only a few, the instance that keeps
// polling among them.
func TestDupTrackerBounded(t *testing.T) {
	d := newDupTracker()
	t0 := time.Date(2026, 11, 28, 9, 0, 0, 0, time.UTC)
	long := strings.Repeat("x", 1<<20)
	for i := 0; i < 200; i++ {
		now := t0.Add(time.Duration(i) * time.Second)
		d.seen(9, "orig", "srv", "10.0.0.9", now, testQuiet)
		d.seen(9, fmt.Sprintf("%d-%s", i, long), long, long, now, testQuiet)
	}
	d.mu.Lock()
	list := d.inst[9]
	found := false
	for _, x := range list {
		if len(x.id) > 64 || len(x.host) > 255 || len(x.ip) > 64 {
			t.Errorf("unclipped instance: %d %d %d bytes", len(x.id), len(x.host), len(x.ip))
		}
		found = found || x.id == "orig"
	}
	d.mu.Unlock()
	if len(list) > dupMaxInstances || !found {
		t.Fatalf("%d instances kept, the polling one among them: %v", len(list), found)
	}
	// Agents that stopped polling are forgotten.
	d.current(t0.Add(time.Hour))
	d.mu.Lock()
	n := len(d.inst) + len(d.since) + len(d.reported)
	d.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d entries kept for agents that stopped polling", n)
	}
	if id := legacyInstance("host", []string{strings.Repeat("1", 300)}); len(id) > 64 || legacyInstance("host", []string{strings.Repeat("1", 299)}) == id {
		t.Fatalf("legacy instance %q", id)
	}
}
