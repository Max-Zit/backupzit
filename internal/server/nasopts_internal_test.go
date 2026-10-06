package server

import "testing"

// The stored NAS password stays when only the share or folder changes on
// the same server, and is never carried over to another server.
func TestSameNASServer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"smb://nas01/data", "smb://nas01/backups", true},
		{"smb://NAS01/data", "smb://nas01/data", true},
		{"smb://nas01/data", "smb://nas02/data", false},
		{"smb://nas01/data", "nfs://nas01/data", false},
		{"smb://nas01/data", "", false},
	} {
		if got := sameNASServer(c.a, c.b); got != c.want {
			t.Errorf("sameNASServer(%q, %q) = %v", c.a, c.b, got)
		}
	}
}
