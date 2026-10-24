//go:build !windows

package agent

import (
	"strings"
	"testing"
)

func TestUninstallScript(t *testing.T) {
	all := func(string) bool { return true }
	none := func(string) bool { return false }
	if s := uninstallScript(all, all); !strings.Contains(s, "dpkg -r backupzit-agent") {
		t.Errorf("deb: %q", s)
	}
	if s := uninstallScript(func(n string) bool { return n == "rpm" }, none); !strings.Contains(s, "rpm -e backupzit-agent") {
		t.Errorf("rpm: %q", s)
	}
	if s := uninstallScript(none, none); s != "" {
		t.Errorf("no package manager: %q", s)
	}
}
