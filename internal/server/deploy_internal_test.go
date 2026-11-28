package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/update"
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
	pkg, err := s.agentPackage("deb")
	if err != nil || pkg.name != "backupzit-agent_0.29.0_amd64.deb" || pkg.sum != "11507a0e2f5e69d5dfa40a62a1bd7b6ee57e6bcd85c67c9b8431b36fff21c437" || pkg.version != "0.29.0" || pkg.signed {
		t.Errorf("deb: %+v %v", pkg, err)
	}
	p := pkg.path
	if p, _ := s.agentPackage("msi"); p.name != "backupzit-agent-0.29.0-x64.msi" {
		t.Errorf("msi: %s", p.name)
	}
	if p, _ := s.agentPackage("legacy-msi"); p.name != "backupzit-agent-0.29.0-x64-legacy.msi" {
		t.Errorf("legacy: %s", p.name)
	}
	if _, err := s.agentPackage("rpm"); err == nil {
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

// MED 12: push-install takes the package of the newest version (not the
// newest file), checks it against the signed list of that release like an
// agent update, and never puts an odd file name into its root script.
func TestAgentPackageSignedList(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	saved := update.TrustedKeys
	update.TrustedKeys = []string{base64.StdEncoding.EncodeToString(pub)}
	defer func() { update.TrustedKeys = saved }()
	dist := t.TempDir()
	s := &Server{DistDir: dist}
	write := func(name, body string, age time.Duration) {
		p := filepath.Join(dist, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, time.Now().Add(-age), time.Now().Add(-age))
	}
	write("backupzit-agent_0.62.0_amd64.deb", "agent 0.62.0", time.Hour)
	write("backupzit-agent_0.61.0_amd64.deb", "agent 0.61.0", 0) // older version, newer file
	write("backupzit-agent_0.99.0_amd64'$(id).deb", "planted", 0)
	pkg, err := s.agentPackage("deb")
	if err != nil || pkg.name != "backupzit-agent_0.62.0_amd64.deb" || pkg.version != "0.62.0" || pkg.signed {
		t.Fatalf("by version, odd names left out: %+v %v", pkg, err)
	}
	manifest := func(kind, content string) {
		sum := sha256.Sum256([]byte(content))
		raw, _ := json.Marshal(update.Manifest{Product: "backupzit", Version: "0.62.0", Files: []update.File{{Name: "backupzit-agent_0.62.0_amd64.deb",
			Kind: kind, Format: "deb", Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:])}}})
		list := filepath.Join(dist, update.AgentManifestName("0.62.0"))
		os.WriteFile(list, raw, 0o644)
		os.WriteFile(list+".sig", []byte(update.Sign(raw, key)+"\n"), 0o644)
	}
	manifest("agent", "agent 0.62.0")
	if pkg, err := s.agentPackage("deb"); err != nil || !pkg.signed {
		t.Fatalf("listed package: %+v %v", pkg, err)
	}
	manifest("repo", "agent 0.62.0")
	if _, err := s.agentPackage("deb"); err == nil || !strings.Contains(err.Error(), "not an agent installer") {
		t.Errorf("a repository package installed as an agent: %v", err)
	}
	manifest("agent", "agent 0.62.0")
	write("backupzit-agent_0.62.0_amd64.deb", "agent 0.62.0, changed", time.Hour)
	if _, err := s.agentPackage("deb"); err == nil || !strings.Contains(err.Error(), "not the one of release 0.62.0") {
		t.Errorf("a changed package was installed: %v", err)
	}
	os.Remove(filepath.Join(dist, update.AgentManifestName("0.62.0")+".sig"))
	if _, err := s.agentPackage("deb"); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Errorf("a list without its signature was used: %v", err)
	}
}

// MED 15a: agents from 0.63 get the enrollment code on their input, older
// and unversioned packages as an argument (they do not know "--code -").
func TestLinuxEnrollCommand(t *testing.T) {
	if got := linuxEnroll("BZ1-abc", "0.63.0"); got != "backupzit-agent enroll --force --code - <<'BZCODE'\nBZ1-abc\nBZCODE" {
		t.Errorf("0.63: %q", got)
	}
	for _, v := range []string{"0.62.2", ""} {
		if got := linuxEnroll("BZ1-abc", v); got != "backupzit-agent enroll --force --code 'BZ1-abc'" {
			t.Errorf("%q: %q", v, got)
		}
	}
}

// LOW: an installation that ended keeps its log and result but drops the
// passwords; finished installations are forgotten after a day, and expired
// confirmations go without waiting for the next one.
func TestDeploySecretsDropped(t *testing.T) {
	q := deployRequest{Password: "login-pw-1", SudoPassword: "sudo-pw-2"}
	d := &deployment{ID: "a", secrets: q.secrets()}
	if msg := d.finish(errors.New("sudo: wrong password sudo-pw-2")); strings.Contains(msg, "sudo-pw-2") {
		t.Errorf("error not hidden: %q", msg)
	}
	v := d.view()
	if d.secrets != nil || !v.Done || !strings.Contains(v.Err, "[hidden]") || !strings.Contains(v.Log[len(v.Log)-1], "FAILED") {
		t.Fatalf("after the end: secrets %v, view %+v", d.secrets, v)
	}
	running := &deployment{ID: "b", secrets: []string{"x"}}
	old := &deployment{ID: "c", done: true, ended: time.Now().Add(-25 * time.Hour)}
	dp := &deployer{runs: map[string]*deployment{"a": d, "b": running, "c": old}}
	dp.prune(time.Now())
	if _, ok := dp.runs["c"]; ok || len(dp.runs) != 2 {
		t.Errorf("after pruning: %v", dp.runs)
	}

	ps := &pendingStore{m: map[string]*pendingConnect{}}
	id := ps.add(&pendingConnect{By: "admin", Deploy: &q})
	other := ps.add(&pendingConnect{By: "admin", Deploy: &q})
	ps.m[id].Expires = time.Now().Add(-time.Second)
	if _, err := ps.take(other, "admin"); err != nil {
		t.Fatal(err)
	}
	if len(ps.m) != 0 {
		t.Errorf("an expired confirmation is kept: %d", len(ps.m))
	}
}
