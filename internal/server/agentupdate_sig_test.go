package server_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/update"
)

// GHSA-v284-8576-jp84: the console sends the signed list of the agent
// installers with an update and refuses an installer that differs from it.
func TestAgentUpdateSignedList(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	saved := update.TrustedKeys
	update.TrustedKeys = append([]string{base64.StdEncoding.EncodeToString(pub)}, saved...)
	defer func() { update.TrustedKeys = saved }()
	dist := t.TempDir()
	e.srv.DistDir = dist
	deb := []byte("agent package 0.57.0")
	name := "backupzit-agent_0.57.0_amd64.deb"
	if err := os.WriteFile(filepath.Join(dist, name), deb, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(deb)
	raw, _ := json.Marshal(update.Manifest{Product: "backupzit", Version: "0.57.0", Files: []update.File{{Name: name, Kind: "agent", Format: "deb", Size: int64(len(deb)), SHA256: hex.EncodeToString(sum[:])}}})
	list := filepath.Join(dist, update.AgentManifestName("0.57.0"))
	os.WriteFile(list, raw, 0o600)
	os.WriteFile(list+".sig", []byte(update.Sign(raw, key)+"\n"), 0o600)
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "0.56.0")
	if err != nil {
		t.Fatal(err)
	}
	var agentID int64
	agents, _ := e.store.ListAgents(ctx)
	for _, a := range agents {
		if a.UUID == cfg.AgentUUID {
			agentID = a.ID
		}
	}
	queue := func() int64 {
		var id int64
		if err := e.pool.QueryRow(ctx, `INSERT INTO runs(agent_id, kind, trigger, repo_url, paths) VALUES ($1,'agent-update','manual','',$2) RETURNING id`,
			agentID, []string{name, "0.57.0"}).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	ar, err := e.srv.APIRun(ctx, queue())
	if err != nil || ar.UpdateManifest != string(raw) || ar.UpdateManifestSig == "" || ar.UpdateSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("update run %+v: %v", ar, err)
	}
	if _, err := update.VerifyAgentInstaller([]byte(ar.UpdateManifest), ar.UpdateManifestSig, nil, "0.57.0", name); err != nil {
		t.Fatalf("the list the agent gets does not verify: %v", err)
	}
	// Someone with access to the console swaps the installer.
	os.WriteFile(filepath.Join(dist, name), []byte("something else"), 0o600)
	if _, err := e.srv.APIRun(ctx, queue()); err == nil || !strings.Contains(err.Error(), "not the one of release 0.57.0") {
		t.Fatalf("a swapped installer was handed out: %v", err)
	}
}
