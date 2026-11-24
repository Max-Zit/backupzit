package server

import (
	"os"
	"path/filepath"
	"testing"
)

// Each agent is offered the installer of its own system and architecture;
// a Mac gets the universal package (Apple silicon and Intel).
func TestPackageKind(t *testing.T) {
	for _, c := range []struct {
		os, arch, want string
	}{
		{"Windows 11 Pro (build 26100)", "amd64", "msi"},
		{"Ubuntu 24.04.1 LTS", "amd64", "deb"},
		{"Rocky Linux 9.4 (Blue Onyx)", "amd64", "rpm"},
		{"Ubuntu 24.04.1 LTS", "arm64", ""}, // no Linux arm64 packages
		{"macOS 27.0.1 (26A434)", "arm64", "pkg"},
		{"macOS 26.5.2 (25F79)", "amd64", "pkg"}, // Intel Macs too, since 0.60
		{"macOS 26.5.2 (25F79)", "386", ""},
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
	if got := fileKind("backupzit-agent_0.60.0_darwin_universal.pkg"); got != "pkg" {
		t.Errorf("fileKind: %q", got)
	}
	// The Apple silicon packages of 0.59 are not offered: an Intel Mac would
	// get one it cannot run.
	for _, n := range []string{"backupzit-agent_0.59.0_darwin_arm64.pkg", "backupzit-agent_0.59.0_darwin_amd64.pkg", "other_0.60.0_darwin_universal.pkg"} {
		if got := fileKind(n); got != "" {
			t.Errorf("fileKind(%s): %q", n, got)
		}
	}
	title, _, _, _, ok := classify("backupzit-agent_0.60.0_darwin_universal.pkg")
	if !ok || title != "macOS agent" {
		t.Errorf("classify: %q %v", title, ok)
	}
}

// A Mac running an older agent is offered the newest .pkg, and only that.
func TestAvailableUpdatesForMac(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{
		"backupzit-agent_0.59.0_darwin_arm64.pkg",
		"backupzit-agent_0.60.0_darwin_universal.pkg",
		"backupzit-agent-0.61.0-x64.msi",
	} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{DistDir: dir}
	mac := Agent{ID: 1, OS: "macOS 27.0.1 (26A434)", Arch: "arm64", Version: "0.59.1"}
	current := Agent{ID: 2, OS: "macOS 27.0.1 (26A434)", Arch: "arm64", Version: "0.60.0"}
	intel := Agent{ID: 3, OS: "macOS 26.5.2 (25F79)", Arch: "amd64", Version: "0.59.9"}
	got := s.availableUpdates([]Agent{mac, current, intel})
	if u := got[1]; u.File != "backupzit-agent_0.60.0_darwin_universal.pkg" || u.Version != "0.60.0" {
		t.Errorf("older Mac: %+v", u)
	}
	if u := got[3]; u.File != "backupzit-agent_0.60.0_darwin_universal.pkg" {
		t.Errorf("Intel Mac: %+v", u)
	}
	if u, ok := got[2]; ok {
		t.Errorf("up-to-date Mac offered %+v", u)
	}
}

// Downloads of the same position keep one order (by title): the macOS
// package after the rpm, whatever order the folder lists them in.
func TestDownloadsOrderStable(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"backupzit-agent_0.59.0_darwin_universal.pkg", "backupzit-agent-0.59.0-1.x86_64.rpm", "backupzit-agent_0.59.0_amd64.deb"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{DistDir: dir}
	for range 20 {
		d := s.downloads()
		if len(d) != 3 || d[1].Title != "Linux agent — AlmaLinux / Rocky Linux" || d[2].Title != "macOS agent" {
			t.Fatalf("order: %+v", d)
		}
	}
}
