package hyperv

import "testing"

func TestCrashConsistentExpected(t *testing.T) {
	list := []string{"old-centos", "legacy-*", "  Appliance?  "}
	for name, want := range map[string]bool{
		"old-centos": true, "OLD-CentOS": true, "legacy-web01": true, "appliance1": true,
		"sql01": false, "old-centos2": false, "appliance12": false, "": false,
	} {
		if got := CrashConsistentExpected(name, list); got != want {
			t.Errorf("%q: %v, want %v", name, got, want)
		}
	}
	if CrashConsistentExpected("x", []string{"[bad"}) {
		t.Error("a broken pattern matched")
	}
}
