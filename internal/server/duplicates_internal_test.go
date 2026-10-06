package server

import (
	"testing"
	"time"
)

func TestDupTracker(t *testing.T) {
	d := newDupTracker()
	t0 := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	poll := func(inst, host string, at time.Duration) (bool, bool) {
		return d.seen(7, inst, host, "10.0.0.1", t0.Add(at))
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
	if dup, _ := d.seen(8, "", "old", "10.0.0.2", t0); dup {
		t.Fatal("agent without instance reported")
	}
}
