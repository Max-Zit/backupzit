package agent

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/update"
)

// GHSA-v284-8576-jp84: an agent installs only what the signed release list
// names, checked with its own release keys; the console's checksum alone
// is not enough, and agents are never downgraded.
func TestSignedInstaller(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	updateKeys = []string{base64.StdEncoding.EncodeToString(pub)}
	defer func() { updateKeys = nil }()
	const sum = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	list := func(version string, k ed25519.PrivateKey) (string, string) {
		raw, _ := json.Marshal(update.Manifest{Product: "backupzit", Version: version, Files: []update.File{
			{Name: "backupzit-agent_0.56.0_amd64.deb", Kind: "agent", Format: "deb", Size: 10, SHA256: sum},
			{Name: "backupzit-server_0.56.0_amd64.deb", Kind: "server", Format: "deb", Size: 10, SHA256: sum},
		}})
		return string(raw), update.Sign(raw, k)
	}
	run := func(version, file string, k ed25519.PrivateKey) api.Run {
		m, s := list(version, k)
		return api.Run{Kind: api.KindAgentUpdate, UpdateFile: file, UpdateVersion: "0.56.0", UpdateSHA256: sum, UpdateManifest: m, UpdateManifestSig: s}
	}
	if f, err := signedInstaller(run("0.56.0", "backupzit-agent_0.56.0_amd64.deb", key), "0.55.0"); err != nil || f.SHA256 != sum {
		t.Fatalf("a signed update was refused: %v", err)
	}
	if _, err := signedInstaller(run("0.56.0", "backupzit-agent_0.56.0_amd64.deb", key), "dev"); err != nil {
		t.Fatalf("a development build cannot update: %v", err)
	}
	for name, tc := range map[string]struct {
		r    api.Run
		own  string
		want string
	}{
		"no list (older console)": {api.Run{UpdateFile: "backupzit-agent_0.56.0_amd64.deb", UpdateVersion: "0.56.0", UpdateSHA256: sum}, "0.55.0", "no signed list"},
		"another key":             {run("0.56.0", "backupzit-agent_0.56.0_amd64.deb", otherKey), "0.55.0", "release key"},
		"list of another version": {run("0.55.9", "backupzit-agent_0.56.0_amd64.deb", key), "0.55.0", "not 0.56.0"},
		"installer not listed":    {run("0.56.0", "evil.deb", key), "0.55.0", "not in the signed list"},
		"the server package":      {run("0.56.0", "backupzit-server_0.56.0_amd64.deb", key), "0.55.0", "not in the signed list"},
		"downgrade":               {run("0.56.0", "backupzit-agent_0.56.0_amd64.deb", key), "0.57.1", "not newer"},
	} {
		if _, err := signedInstaller(tc.r, tc.own); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want %q", name, err, tc.want)
		}
	}
	// A changed list fails the signature.
	r := run("0.56.0", "backupzit-agent_0.56.0_amd64.deb", key)
	r.UpdateManifest = strings.Replace(r.UpdateManifest, sum, strings.Repeat("f", 64), 1)
	if _, err := signedInstaller(r, "0.55.0"); err == nil {
		t.Error("a changed list was accepted")
	}
}
