package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/update"
)

// testKey writes a release key and makes it the only trusted one.
func testKey(t *testing.T) (string, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "release.key")
	if err := os.WriteFile(p, []byte(base64.StdEncoding.EncodeToString(priv.Seed())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	saved := update.TrustedKeys
	update.TrustedKeys = []string{base64.StdEncoding.EncodeToString(pub)}
	t.Cleanup(func() { update.TrustedKeys = saved })
	return p, priv
}

func signedRelease(t *testing.T, key string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "backupzit-server_0.56.0_amd64.deb"), []byte("package"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"sign", "-key", key, "-version", "0.56.0", "-dir", dir}); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestNotesVerifiesBeforeSigning(t *testing.T) {
	key, _ := testKey(t)
	notes := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(notes, []byte("New notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir := signedRelease(t, key)
	if err := run([]string{"notes", "-key", key, "-dir", dir, "-notes", notes, "-version", "0.56.0"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	sig, _ := os.ReadFile(filepath.Join(dir, "manifest.json.sig"))
	m, err := update.Verify(raw, string(sig), nil)
	if err != nil || m.Notes != "New notes" {
		t.Fatalf("%+v %v", m, err)
	}

	// A changed checksum, a missing signature or another version's
	// manifest is not signed again.
	for name, mutate := range map[string]func(dir string){
		"tampered": func(dir string) {
			raw, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
			s := string(raw)
			i := strings.Index(s, `"sha256": "`) + len(`"sha256": "`)
			c := "0"
			if s[i] == '0' {
				c = "1"
			}
			_ = os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(s[:i]+c+s[i+1:]), 0o644)
		},
		"no signature":  func(dir string) { _ = os.Remove(filepath.Join(dir, "manifest.json.sig")) },
		"other version": func(dir string) {},
	} {
		dir := signedRelease(t, key)
		mutate(dir)
		before, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
		want := "0.56.0"
		if name == "other version" {
			want = "0.57.0"
		}
		if err := run([]string{"notes", "-key", key, "-dir", dir, "-notes", notes, "-version", want}); err == nil {
			t.Errorf("%s: signed again", name)
		}
		if after, _ := os.ReadFile(filepath.Join(dir, "manifest.json")); string(after) != string(before) {
			t.Errorf("%s: manifest changed", name)
		}
	}
}

func TestSignFile(t *testing.T) {
	key, priv := testKey(t)
	p := filepath.Join(t.TempDir(), "SHA256SUMS")
	content := []byte("0123  backupzit-appliance.iso\n")
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"sign-file", "-key", key, p}); err != nil {
		t.Fatal(err)
	}
	sig, err := os.ReadFile(p + ".sig")
	if err != nil {
		t.Fatal(err)
	}
	s, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil || !ed25519.Verify(priv.Public().(ed25519.PublicKey), content, s) {
		t.Fatalf("signature does not verify: %v", err)
	}

	// A key that consoles do not trust signs nothing.
	update.TrustedKeys = []string{base64.StdEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize))}
	_ = os.Remove(p + ".sig")
	if err := run([]string{"sign-file", "-key", key, p}); err == nil {
		t.Error("signed with an untrusted key")
	}
	if _, err := os.Stat(p + ".sig"); err == nil {
		t.Error("signature written with an untrusted key")
	}
}
