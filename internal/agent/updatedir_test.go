//go:build !windows

package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// The update folder must be the agent's own: a folder another user made
// first (in /var/tmp before 0.54.1), or a link, is refused; a folder that
// others could write gets 0700.
func TestPrivateUpdateDir(t *testing.T) {
	if updateDir() != "/var/lib/backupzit-agent/updates" {
		t.Errorf("update folder %s", updateDir())
	}
	base := t.TempDir()
	own := filepath.Join(base, "own")
	if err := os.Mkdir(own, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(own, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := privateDir(own); err != nil {
		t.Fatalf("own folder: %v", err)
	}
	if fi, _ := os.Stat(own); fi.Mode().Perm() != 0o700 {
		t.Errorf("own folder mode %v, want 0700", fi.Mode().Perm())
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(own, link); err != nil {
		t.Fatal(err)
	}
	if err := privateDir(link); err == nil {
		t.Error("a link was accepted as the update folder")
	}
	if os.Geteuid() == 0 {
		other := filepath.Join(base, "other")
		if err := os.Mkdir(other, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(other, 65534, 65534); err != nil {
			t.Fatal(err)
		}
		if err := privateDir(other); err == nil {
			t.Error("a folder of another user was accepted")
		}
	}
}
