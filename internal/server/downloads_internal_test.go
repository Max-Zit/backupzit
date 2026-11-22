package server

import (
	"os"
	"path/filepath"
	"testing"
)

// Each agent is offered the installer of its own system and architecture;
// a Mac gets the Apple silicon package (issue #2).
func TestPackageKind(t *testing.T) {
	for _, c := range []struct {
		os, arch, want string
	}{
		{"Windows 11 Pro (build 26100)", "amd64", "msi"},
		{"Ubuntu 24.04.1 LTS", "amd64", "deb"},
		{"Rocky Linux 9.4 (Blue Onyx)", "amd64", "rpm"},
		{"Ubuntu 24.04.1 LTS", "arm64", ""}, // no Linux arm64 packages
		{"macOS 27.0.1 (26A434)", "arm64", "pkg"},
		{"macOS 14.6 (23G80)", "amd64", ""}, // Intel Macs: no package
		{"", "amd64", ""},
	} {
		if got := packageKind(Agent{OS: c.os, Arch: c.arch}); got != c.want {
			t.Errorf("%s/%s: %q, want %q", c.os, c.arch, got, c.want)
		}
	}
	if got := packageKind(Agent{OS: "macOS 27.0.1", Arch: "arm64", Recovery: true}); got != "" {
		t.Errorf("recovery agent: %q", got)
	}
}

func TestFileKindAndDownloadsOfMacPackage(t *testing.T) {
	if got := fileKind("backupzit-agent_0.59.0_darwin_arm64.pkg"); got != "pkg" {
		t.Errorf("fileKind: %q", got)
	}
	for _, n := range []string{"backupzit-agent_0.59.0_darwin_amd64.pkg", "other_0.59.0_darwin_arm64.pkg"} {
		if got := fileKind(n); got != "" {
			t.Errorf("fileKind(%s): %q", n, got)
		}
	}
	title, _, _, _, ok := classify("backupzit-agent_0.59.0_darwin_arm64.pkg")
	if !ok || title != "macOS agent — Apple Silicon" {
		t.Errorf("classify: %q %v", title, ok)
	}
}

// A Mac running an older agent is offered the newest .pkg, and only that.
func TestAvailableUpdatesForMac(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{
		"backupzit-agent_0.58.1_darwin_arm64.pkg",
		"backupzit-agent_0.59.0_darwin_arm64.pkg",
		"backupzit-agent-0.60.0-x64.msi",
	} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{DistDir: dir}
	mac := Agent{ID: 1, OS: "macOS 27.0.1 (26A434)", Arch: "arm64", Version: "0.58.0"}
	current := Agent{ID: 2, OS: "macOS 27.0.1 (26A434)", Arch: "arm64", Version: "0.59.0"}
	got := s.availableUpdates([]Agent{mac, current})
	if u := got[1]; u.File != "backupzit-agent_0.59.0_darwin_arm64.pkg" || u.Version != "0.59.0" {
		t.Errorf("older Mac: %+v", u)
	}
	if u, ok := got[2]; ok {
		t.Errorf("up-to-date Mac offered %+v", u)
	}
}
