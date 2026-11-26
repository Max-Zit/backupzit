package agent

import (
	"path/filepath"
	"testing"
)

func TestParseNASShare(t *testing.T) {
	for raw, want := range map[string]nasShare{
		"smb://nas01/data":             {Proto: "smb", Host: "nas01", Share: "data"},
		"smb://nas01/data/Scans/2026/": {Proto: "smb", Host: "nas01", Share: "data", Sub: "Scans/2026"},
		"nfs://10.0.0.5/volume1/data":  {Proto: "nfs", Host: "10.0.0.5", Share: "/volume1/data"},
	} {
		got, err := parseNASShare(raw)
		if err != nil || got != want {
			t.Errorf("%s: %+v %v", raw, got, err)
		}
	}
	for _, bad := range []string{"smb://nas01", "nfs://nas01/", "ftp://nas01/x", "nas01/data",
		"smb://nas01/data/../../etc", "smb://nas01/data/%2e%2e/x", "smb://nas01/./data", "nfs://nas01/volume1/../etc", `smb://nas01/data\..\x`} { // A12
		if _, err := parseNASShare(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestNASPaths(t *testing.T) {
	root := filepath.FromSlash("/mnt/backupzit-nas/nas01/data")
	got, err := nasPaths(root, []string{"Projects", `Scans\2026`, "/a/"})
	if err != nil || len(got) != 3 || got[1] != filepath.Join(root, "Scans", "2026") || got[2] != filepath.Join(root, "a") {
		t.Errorf("%v %v", got, err)
	}
	if got, _ := nasPaths(root, nil); len(got) != 1 || got[0] != root {
		t.Errorf("whole share: %v", got)
	}
	if _, err := nasPaths(root, []string{"../etc"}); err == nil {
		t.Error("path outside the share accepted")
	}
}

func TestBelowMount(t *testing.T) {
	m := filepath.FromSlash("/mnt/nas")
	for p, want := range map[string]bool{"/mnt/nas": true, "/mnt/nas/a/b": true, "/mnt/nasx": false, "/mnt": false, "/etc": false} {
		if got := belowMount(filepath.FromSlash(p), m); got != want {
			t.Errorf("belowMount(%s) = %v", p, got)
		}
	}
}
