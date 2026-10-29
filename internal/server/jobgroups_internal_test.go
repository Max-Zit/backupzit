package server

import "testing"

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
