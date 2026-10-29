package server

import (
	"strings"
	"testing"
	"time"
)

// The scopes of a job for several agents use the operating system names
// that agents really report.
func TestGroupScopeMatches(t *testing.T) {
	agents := map[string]string{
		"Windows 11 Enterprise Evaluation (build 26300)":        GroupWindows,
		"Windows Server 2022 Standard Evaluation (build 20348)": GroupWindows,
		"Debian GNU/Linux 12 (bookworm)":                        GroupLinux,
		"Ubuntu 24.04.1 LTS":                                    GroupLinux,
		"Rocky Linux 9.4 (Blue Onyx)":                           GroupLinux,
		"darwin":                                                GroupMacOS,
	}
	id := int64(0)
	for os, want := range agents {
		id++
		a := Agent{ID: id, OS: os}
		for _, scope := range []string{GroupWindows, GroupLinux, GroupMacOS} {
			if got := (JobGroup{Scope: scope}).Matches(a); got != (scope == want) {
				t.Errorf("%q in scope %s: %v", os, scope, got)
			}
		}
		if !(JobGroup{Scope: GroupAll}).Matches(a) || (JobGroup{Scope: GroupAll, ExcludeIDs: []int64{id}}).Matches(a) {
			t.Errorf("%q: all agents / left out", os)
		}
		if (JobGroup{Scope: GroupWindows}).Matches(Agent{ID: id, OS: os, Recovery: true}) {
			t.Errorf("%q: recovery agent matched", os)
		}
	}
}

// Disk image groups cover Windows agents, Linux system groups Linux agents;
// their members get all disks or the whole system.
func TestGroupKinds(t *testing.T) {
	win, lin := Agent{ID: 1, OS: "Windows 11 Pro"}, Agent{ID: 2, OS: "Ubuntu 24.04.1 LTS"}
	img := JobGroup{Kind: JobImage, Scope: GroupAll, Name: "images", TargetID: 1}
	sys := JobGroup{Kind: JobSystem, Scope: GroupAll, Name: "systems", TargetID: 1}
	if err := img.check(); err != nil || len(img.Paths) != 0 {
		t.Fatalf("image check: %v %v", err, img.Paths)
	}
	if err := sys.check(); err != nil || len(sys.Paths) != 1 || sys.Paths[0] != "/" {
		t.Fatalf("system check: %v %v", err, sys.Paths)
	}
	if !img.Matches(win) || img.Matches(lin) || sys.Matches(win) || !sys.Matches(lin) {
		t.Fatal("kind does not limit the operating system")
	}
	chosenImg := JobGroup{Kind: JobImage, Scope: GroupAgents, AgentIDs: []int64{win.ID, lin.ID}}
	chosenSys := JobGroup{Kind: JobSystem, Scope: GroupAgents, AgentIDs: []int64{win.ID, lin.ID}}
	if !chosenImg.Matches(win) || chosenImg.Matches(lin) || chosenSys.Matches(win) || !chosenSys.Matches(lin) {
		t.Fatal("kind does not limit the operating system of chosen agents")
	}
	if m := img.member(win.ID); m.Kind != JobImage || m.ImageDisk == nil || *m.ImageDisk != -1 {
		t.Fatalf("image member: %+v", m)
	}
	for _, g := range []JobGroup{{Kind: JobImage, Scope: GroupLinux, Name: "x"}, {Kind: JobSystem, Scope: GroupWindows, Name: "x"}, {Kind: JobSQL, Scope: GroupAll, Name: "x"}} {
		if err := g.check(); err == nil {
			t.Errorf("%s for %s accepted", g.Kind, g.Scope)
		}
	}
}

// Agents in no enabled job are listed, except recovery agents and agents
// enrolled less than an hour ago.
func TestUnprotectedAgents(t *testing.T) {
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	agents := []Agent{
		{ID: 1, Hostname: "with-job", EnrolledAt: old},
		{ID: 2, Hostname: "only-disabled", EnrolledAt: old},
		{ID: 3, Hostname: "none", EnrolledAt: old},
		{ID: 4, Hostname: "new", EnrolledAt: now.Add(-10 * time.Minute)},
		{ID: 5, Hostname: "recovery", EnrolledAt: old, Recovery: true},
		{ID: 6, Hostname: "vmware-proxy", EnrolledAt: old},
	}
	jobs := []Job{{AgentID: 1, Enabled: true}, {AgentID: 2, Enabled: false}, {AgentID: 6, Enabled: true}}
	var got []string
	for _, a := range unprotectedAgents(agents, jobs, now) {
		got = append(got, a.Hostname)
	}
	if strings.Join(got, ",") != "only-disabled,none" {
		t.Fatalf("unprotected = %v", got)
	}
}
