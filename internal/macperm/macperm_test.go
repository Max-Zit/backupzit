package macperm

import (
	"runtime"
	"testing"
)

// The answer depends on the permissions of the process running the tests;
// it must only be a valid one for the system.
func TestFullDiskAccessState(t *testing.T) {
	s := FullDiskAccess()
	if runtime.GOOS != "darwin" {
		if s != NotApplicable {
			t.Fatalf("%s on %s", s, runtime.GOOS)
		}
		return
	}
	switch s {
	case Granted, Denied, Unknown:
		t.Logf("Full Disk Access: %s", s)
	default:
		t.Fatalf("state %q", s)
	}
}
