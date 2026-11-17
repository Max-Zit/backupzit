package sysbackup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSelinuxActive(t *testing.T) {
	dir := t.TempDir()
	for content, want := range map[string]bool{
		"# comment\nSELINUX=enforcing\nSELINUXTYPE=targeted\n": true,
		"SELINUX=permissive\n":                                 true,
		"SELINUX = \"Enforcing\"\n":                            true,
		"SELINUX=disabled\n":                                   false,
		"#SELINUX=enforcing\nSELINUX=disabled\n":               false,
		"SELINUXTYPE=targeted\n":                               false,
	} {
		p := filepath.Join(dir, "config")
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := selinuxActive(p); got != want {
			t.Errorf("%q: %v, want %v", content, got, want)
		}
	}
	if selinuxActive(filepath.Join(dir, "missing")) {
		t.Error("missing config counts as active")
	}
}
