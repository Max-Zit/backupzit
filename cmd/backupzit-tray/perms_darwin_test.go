package main

import (
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/localipc"
)

func status(version, fda string) *localipc.Status {
	st := &localipc.Status{Version: version}
	if fda != "" {
		st.MacPermissions = &localipc.MacPermissions{FullDiskAccess: fda, AgentPath: "/Library/BackupZit/backupzit-agent"}
	}
	return st
}

// The menu shows the agent's Full Disk Access (not the app's) and what the
// app checks itself.
func TestPermTitles(t *testing.T) {
	p := permsFrom(status("0.58", "denied"), permGranted, permAbsent)
	want := map[permKey]string{
		keyAgent:  "✅ Agent allowed in the background",
		keyFDA:    "❌ Full Disk Access for the agent — click to fix",
		keyNotify: "✅ Notifications",
		keyLogin:  "➖ Open at login (not installed)",
	}
	for k, w := range want {
		if got := p.title(k); got != w {
			t.Errorf("%s: %q, want %q", k, got, w)
		}
	}
	if !p.missing() || p.header() != "Permissions — 1 missing" {
		t.Errorf("missing %v, header %q", p.missing(), p.header())
	}

	// Agent not reachable: its permissions cannot be known.
	p = permsFrom(nil, permAsk, permDenied)
	if got := p.title(keyFDA); got != "❔ Full Disk Access for the agent (agent not reachable)" {
		t.Errorf("fda %q", got)
	}
	if got := p.header(); got != "Permissions — 3 missing" {
		t.Errorf("header %q", got)
	}
	// An agent too old to report it.
	if got := permsFrom(status("0.57", ""), permGranted, permGranted).title(keyFDA); got != "❔ Full Disk Access for the agent (unknown)" {
		t.Errorf("old agent %q", got)
	}
	p = permsFrom(status("0.58", "granted"), permUnknown, permGranted)
	if p.missing() || p.header() != "Permissions" {
		t.Errorf("all granted: missing %v, header %q", p.missing(), p.header())
	}
}

// First launch: one dialog per missing permission, step by step, each
// after the previous one is done (or after waitStep).
func TestGuideStepByStep(t *testing.T) {
	now := time.Now()
	g := newGuide(guideMemory{})
	p := permsFrom(status("0.58", "denied"), permDenied, permDenied)

	d, _ := g.next(p, now)
	if d == nil || d.key != keyFDA || !strings.Contains(d.text, "/Library/BackupZit/backupzit-agent") {
		t.Fatalf("first dialog %+v", d)
	}
	if d2, _ := g.next(p, now); d2 != nil {
		t.Fatalf("second dialog while the first is open: %+v", d2)
	}
	g.answered(d, true, now)
	if d, _ := g.next(p, now.Add(10*time.Second)); d != nil {
		t.Fatalf("dialog while the user is in System Settings: %+v", d)
	}
	// Full Disk Access granted: the next step comes.
	p.state[keyFDA] = permGranted
	d, _ = g.next(p, now.Add(20*time.Second))
	if d == nil || d.key != keyLogin {
		t.Fatalf("login dialog %+v", d)
	}
	g.answered(d, true, now)
	// Not done after waitStep: the next one comes anyway.
	d, _ = g.next(p, now.Add(waitStep+time.Second))
	if d == nil || d.key != keyNotify {
		t.Fatalf("notifications dialog %+v", d)
	}
	g.answered(d, true, now)
	// Each is shown once per launch.
	if d, _ := g.next(p, now.Add(2*waitStep)); d != nil {
		t.Fatalf("repeated dialog %+v", d)
	}
}

// "Later" silences the assistant for the rest of the launch.
func TestGuideLater(t *testing.T) {
	now := time.Now()
	g := newGuide(guideMemory{})
	p := permsFrom(status("0.58", "denied"), permDenied, permGranted)
	d, _ := g.next(p, now)
	if d == nil {
		t.Fatal("no dialog")
	}
	g.answered(d, false, now)
	if d, _ := g.next(p, now.Add(time.Hour)); d != nil {
		t.Fatalf("dialog after Later: %+v", d)
	}
}

// Nothing missing, or nothing known: no dialog; notifications not asked
// yet are requested from macOS once instead.
func TestGuideNothingToSay(t *testing.T) {
	g := newGuide(guideMemory{})
	if d, _ := g.next(permsFrom(nil, permUnknown, permAbsent), time.Now()); d != nil {
		t.Fatalf("agent offline: %+v", d)
	}
	p := permsFrom(status("0.58", "granted"), permAsk, permGranted)
	if d, _ := g.next(p, time.Now()); d != nil {
		t.Fatalf("not determined: %+v", d)
	}
	if !g.requestNotifications(p) || g.requestNotifications(p) {
		t.Fatal("notifications must be requested exactly once")
	}
}

// Full Disk Access lost after an update is explained again, even after
// "Later", once per agent version, and remembered across launches.
func TestGuideFDALost(t *testing.T) {
	now := time.Now()
	g := newGuide(guideMemory{})
	if _, changed := g.next(permsFrom(status("0.58", "granted"), permGranted, permGranted), now); !changed || g.mem.FDAGrantedVersion != "0.58" {
		t.Fatalf("granted version not remembered: %+v", g.mem)
	}

	// Next launch, after the update.
	g = newGuide(g.mem)
	g.snoozed = true
	p := permsFrom(status("0.59", "denied"), permGranted, permGranted)
	d, changed := g.next(p, now)
	if d == nil || d.key != keyFDA || !changed || !strings.Contains(d.text, "version 0.59") || !strings.Contains(d.text, "click − to remove it") {
		t.Fatalf("loss dialog %+v changed %v", d, changed)
	}
	g.answered(d, false, now)
	if d, _ := g.next(p, now.Add(time.Minute)); d != nil {
		t.Fatalf("loss shown twice: %+v", d)
	}
	// Following launch, same version: the regular guidance, not the loss.
	g = newGuide(g.mem)
	d, _ = g.next(p, now)
	if d == nil || strings.Contains(d.text, "since its update") {
		t.Fatalf("regular dialog expected: %+v", d)
	}
	// Granted again, then lost again with the next update.
	g.answered(d, true, now)
	p = permsFrom(status("0.59", "granted"), permGranted, permGranted)
	g.next(p, now)
	if g.mem.FDAGrantedVersion != "0.59" || g.mem.LossShownVersion != "" {
		t.Fatalf("memory %+v", g.mem)
	}
}

func TestLoginItemState(t *testing.T) {
	cases := []struct {
		exists, started, loaded bool
		want                    permState
	}{
		{false, false, false, permAbsent},
		{false, true, true, permAbsent},
		{true, true, false, permGranted},
		{true, false, true, permGranted},
		{true, false, false, permDenied},
	}
	for _, c := range cases {
		if got := loginItemState(c.exists, c.started, c.loaded); got != c.want {
			t.Errorf("%+v: %s", c, got)
		}
	}
}

func TestNotifyStateAndDialogAnswer(t *testing.T) {
	if notifyState(notifyAllowed) != permGranted || notifyState(notifyDenied) != permDenied ||
		notifyState(notifyNotDetermined) != permAsk || notifyState(notifyUnknown) != permUnknown {
		t.Fatal("notification states")
	}
	cases := map[string]bool{
		"button returned:Open System Settings, gave up:false\n": true,
		"button returned:Later, gave up:false\n":                false,
		"button returned:, gave up:true\n":                      false,
		"":                                                      false,
	}
	for out, want := range cases {
		if got := openChosen(out); got != want {
			t.Errorf("%q: %v", out, got)
		}
	}
}
