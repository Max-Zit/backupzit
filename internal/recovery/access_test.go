package recovery

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testPassphrase = "correct horse battery"

func testPackage() *AccessPackage {
	return &AccessPackage{CreatedBy: "admin", Repositories: []AccessRepo{
		{Name: "pc1", Location: `smb://backup@nas/backups/pc1`, Password: "repo-secret", SMBPassword: "smb-secret"},
		{Name: "pc2", Location: "sftp://bz@host/repo", SFTPKey: "-----BEGIN OPENSSH PRIVATE KEY-----\nxyz\n-----END OPENSSH PRIVATE KEY-----\n"},
	}}
}

func TestAccessPackageRoundTrip(t *testing.T) {
	b, err := SealAccessPackage(testPackage(), testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"repo-secret", "smb-secret", "OPENSSH", "nas"} {
		if strings.Contains(string(b), secret) {
			t.Errorf("file contains %q in plain text", secret)
		}
	}
	p, err := OpenAccessPackage(b, testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Repositories) != 2 || p.Repositories[0].SMBPassword != "smb-secret" || p.Created.IsZero() {
		t.Errorf("decrypted %+v", p)
	}
	if _, err := OpenAccessPackage(b, "wrong passphrase!"); !errors.Is(err, ErrAccessPassphrase) {
		t.Errorf("wrong passphrase: %v", err)
	}
}

func TestAccessPackageTampering(t *testing.T) {
	b, err := SealAccessPackage(testPackage(), testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	var e map[string]any
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(m map[string]any){
		func(m map[string]any) { m["created"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano) },
		func(m map[string]any) { m["time"] = 1.0 },
		func(m map[string]any) {
			c := []byte(m["ciphertext"].(string))
			c[10] ^= 1
			m["ciphertext"] = string(c)
		},
	} {
		m := map[string]any{}
		for k, v := range e {
			m[k] = v
		}
		change(m)
		tb, _ := json.Marshal(m)
		if _, err := OpenAccessPackage(tb, testPassphrase); err == nil {
			t.Errorf("manipulated package opened: %s", tb)
		}
	}
	if _, err := OpenAccessPackage([]byte(`{"format":"other"}`), testPassphrase); err == nil {
		t.Error("foreign file opened")
	}
}

func TestAccessPackageRules(t *testing.T) {
	if _, err := SealAccessPackage(testPackage(), "short"); err == nil {
		t.Error("short passphrase accepted")
	}
	if _, err := SealAccessPackage(&AccessPackage{}, testPassphrase); err == nil {
		t.Error("empty package accepted")
	}
}

func TestFindAccessPackages(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "backupzit")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := testPackage()
	old.Created = time.Now().Add(-48 * time.Hour).UTC()
	for name, p := range map[string]*AccessPackage{"old.bzaccess": old, "new.bzaccess": testPackage()} {
		b, err := SealAccessPackage(p, testPassphrase)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "junk.bzaccess"), []byte("nope"), 0o600)
	got := FindAccessPackages([]string{root, filepath.Join(root, "missing")})
	if len(got) != 2 || filepath.Base(got[0].Path) != "new.bzaccess" {
		t.Errorf("found %+v", got)
	}
}

func TestAccessRepoSourceOpensRepository(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "repo")
	newImageRepo(t, dir, "repo-pw")
	a := AccessRepo{Name: "pc", Location: dir, Password: "repo-pw", SFTPKey: "KEY"}
	src, cleanup, err := a.Source(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(src.Options.SFTPKeyFile); err != nil || string(b) != "KEY" {
		t.Errorf("key file: %q, %v", b, err)
	}
	s, err := Open(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	cleanup()
	if _, err := os.Stat(src.Options.SFTPKeyFile); !os.IsNotExist(err) {
		t.Error("key file not removed")
	}
}
