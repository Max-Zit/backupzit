package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDeployHelpers(t *testing.T) {
	for _, q := range []deployRequest{{Host: "", OS: "linux", User: "u", Password: "p"}, {Host: "h", OS: "mac", User: "u", Password: "p"},
		{Host: "h", OS: "windows", User: "u", Key: "k"}, {Host: "a b", OS: "linux", User: "u", Password: "p"}, {Host: "h", OS: "linux", Password: "p"}} {
		if q.validate() == nil {
			t.Errorf("accepted %+v", q)
		}
	}
	q := deployRequest{Host: " srv ", OS: "windows", User: "admin", Password: "p"}
	if err := q.validate(); err != nil || q.Port != 5985 || q.Host != "srv" {
		t.Errorf("windows defaults: %+v %v", q, err)
	}
	q = deployRequest{Host: "srv", OS: "linux", User: "root", Key: "KEY"}
	if err := q.validate(); err != nil || q.Port != 22 {
		t.Errorf("linux with key: %+v %v", q, err)
	}

	dist := t.TempDir()
	s := &Server{DistDir: dist}
	old := filepath.Join(dist, "backupzit-agent_0.28.0_amd64.deb")
	os.WriteFile(old, []byte("old"), 0o644)
	os.Chtimes(old, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour))
	os.WriteFile(filepath.Join(dist, "backupzit-agent_0.29.0_amd64.deb"), []byte("new"), 0o644)
	os.WriteFile(filepath.Join(dist, "backupzit-agent-0.29.0-x64.msi"), []byte("msi"), 0o644)
	os.WriteFile(filepath.Join(dist, "backupzit-agent-0.29.0-x64-legacy.msi"), []byte("legacy"), 0o644)
	p, sum, err := s.agentPackage("deb")
	if err != nil || filepath.Base(p) != "backupzit-agent_0.29.0_amd64.deb" || sum != "11507a0e2f5e69d5dfa40a62a1bd7b6ee57e6bcd85c67c9b8431b36fff21c437" {
		t.Errorf("deb: %s %s %v", p, sum, err)
	}
	if p, _, _ := s.agentPackage("msi"); filepath.Base(p) != "backupzit-agent-0.29.0-x64.msi" {
		t.Errorf("msi: %s", p)
	}
	if p, _, _ := s.agentPackage("legacy-msi"); filepath.Base(p) != "backupzit-agent-0.29.0-x64-legacy.msi" {
		t.Errorf("legacy: %s", p)
	}
	if _, _, err := s.agentPackage("rpm"); err == nil {
		t.Error("missing rpm not reported")
	}

	// One-time links: only the offered file, at most three downloads.
	tok := s.deployState().offerFile(p)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/deploy/{token}/{name}", s.handleDeployDownload)
	get := func(u string) int {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", u, nil))
		return w.Code
	}
	if c := get("/api/deploy/" + tok + "/other.msi"); c != 404 {
		t.Errorf("other file: %d", c)
	}
	if c := get("/api/deploy/wrong/backupzit-agent-0.29.0-x64-legacy.msi"); c != 404 {
		t.Errorf("wrong token: %d", c)
	}
	for i := 0; i < 3; i++ {
		if c := get("/api/deploy/" + tok + "/backupzit-agent_0.29.0_amd64.deb"); c != 200 {
			t.Errorf("download %d: %d", i, c)
		}
	}
	if c := get("/api/deploy/" + tok + "/backupzit-agent_0.29.0_amd64.deb"); c != 404 {
		t.Errorf("used up link: %d", c)
	}
}
