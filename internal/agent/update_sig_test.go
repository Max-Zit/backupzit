package agent

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"runtime"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/update"
)

// GHSA-v284-8576-jp84: an agent installs only what the signed release list
// names, checked with its own release keys; the console's checksum alone
// is not enough, and agents are never downgraded. MED 14: and only an
// agent installer for its own system and processor.
func TestSignedInstaller(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	updateKeys = []string{base64.StdEncoding.EncodeToString(pub)}
	defer func() { updateKeys = nil }()
	const sum = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	// This system's installer, and one of another system.
	own := map[string]update.File{
		"windows": {Name: "backupzit-agent-0.56.0-x64.msi", Format: "msi", Arch: runtime.GOARCH},
		"linux":   {Name: "backupzit-agent_0.56.0_amd64.deb", Format: "deb", Arch: runtime.GOARCH},
		"darwin":  {Name: "backupzit-agent_0.56.0_darwin_universal.pkg", Format: "pkg", Arch: "universal"},
	}[runtime.GOOS]
	if own.Name == "" {
		t.Skip("no agent installer for " + runtime.GOOS)
	}
	other := update.File{Name: "backupzit-agent_0.56.0_darwin_universal.pkg", Format: "pkg", Arch: "universal"}
	if runtime.GOOS == "darwin" {
		other = update.File{Name: "backupzit-agent-0.56.0-x64.msi", Format: "msi", Arch: "amd64"}
	}
	list := func(version string, k ed25519.PrivateKey) (string, string) {
		raw, _ := json.Marshal(update.Manifest{Product: "backupzit", Version: version, Files: []update.File{
			{Name: own.Name, Kind: "agent", Format: own.Format, Arch: own.Arch, Size: 10, SHA256: sum},
			{Name: other.Name, Kind: "agent", Format: other.Format, Arch: other.Arch, Size: 10, SHA256: sum},
			{Name: "backupzit-agent-other-arch." + own.Format, Kind: "agent", Format: own.Format, Arch: "mips", Size: 10, SHA256: sum},
			{Name: "backupzit-server_0.56.0_amd64.deb", Kind: "server", Format: "deb", Size: 10, SHA256: sum},
			{Name: "backupzit-repo_0.56.0_amd64.deb", Kind: "repo", Format: "deb", Arch: "amd64", Size: 10, SHA256: sum},
		}})
		return string(raw), update.Sign(raw, k)
	}
	run := func(version, file string, k ed25519.PrivateKey) api.Run {
		m, s := list(version, k)
		return api.Run{Kind: api.KindAgentUpdate, UpdateFile: file, UpdateVersion: "0.56.0", UpdateSHA256: sum, UpdateManifest: m, UpdateManifestSig: s}
	}
	if f, err := signedInstaller(run("0.56.0", own.Name, key), "0.55.0"); err != nil || f.SHA256 != sum {
		t.Fatalf("a signed update was refused: %v", err)
	}
	if _, err := signedInstaller(run("0.56.0", own.Name, key), "dev"); err != nil {
		t.Fatalf("a development build cannot update: %v", err)
	}
	for name, tc := range map[string]struct {
		r    api.Run
		own  string
		want string
	}{
		"no list (older console)":       {api.Run{UpdateFile: own.Name, UpdateVersion: "0.56.0", UpdateSHA256: sum}, "0.55.0", "no signed list"},
		"another key":                   {run("0.56.0", own.Name, otherKey), "0.55.0", "release key"},
		"list of another version":       {run("0.55.9", own.Name, key), "0.55.0", "not 0.56.0"},
		"installer not listed":          {run("0.56.0", "evil.deb", key), "0.55.0", "not in the signed list"},
		"the server package":            {run("0.56.0", "backupzit-server_0.56.0_amd64.deb", key), "0.55.0", "not in the signed list"},
		"the repository package":        {run("0.56.0", "backupzit-repo_0.56.0_amd64.deb", key), "0.55.0", "not in the signed list"},
		"another system's installer":    {run("0.56.0", other.Name, key), "0.55.0", "not an agent installer for " + runtime.GOOS},
		"another processor's installer": {run("0.56.0", "backupzit-agent-other-arch."+own.Format, key), "0.55.0", "for mips processors"},
		"downgrade":                     {run("0.56.0", own.Name, key), "0.57.1", "not newer"},
	} {
		if _, err := signedInstaller(tc.r, tc.own); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want %q", name, err, tc.want)
		}
	}
	// A changed list fails the signature.
	r := run("0.56.0", own.Name, key)
	r.UpdateManifest = strings.Replace(r.UpdateManifest, sum, strings.Repeat("f", 64), 1)
	if _, err := signedInstaller(r, "0.55.0"); err == nil {
		t.Error("a changed list was accepted")
	}
}

// The formats and processors each agent accepts.
func TestForThisAgent(t *testing.T) {
	for _, tc := range []struct {
		f            update.File
		goos, goarch string
		ok           bool
	}{
		{update.File{Format: "msi", Arch: "amd64"}, "windows", "amd64", true},
		{update.File{Format: "deb", Arch: "amd64"}, "linux", "amd64", true},
		{update.File{Format: "rpm", Arch: "amd64"}, "linux", "amd64", true},
		{update.File{Format: "rpm"}, "linux", "amd64", true}, // no processor in the list
		{update.File{Format: "pkg", Arch: "universal"}, "darwin", "arm64", true},
		{update.File{Format: "pkg", Arch: "universal"}, "darwin", "amd64", true},
		{update.File{Format: "deb", Arch: "amd64"}, "windows", "amd64", false},
		{update.File{Format: "msi", Arch: "amd64"}, "linux", "amd64", false},
		{update.File{Format: "deb", Arch: "amd64"}, "linux", "arm64", false},
		{update.File{Format: "deb", Arch: "universal"}, "linux", "amd64", false},
		{update.File{Format: "msi", Arch: "amd64"}, "freebsd", "amd64", false},
	} {
		if err := forThisAgent(tc.f, tc.goos, tc.goarch); (err == nil) != tc.ok {
			t.Errorf("%+v on %s/%s: %v", tc.f, tc.goos, tc.goarch, err)
		}
	}
}
