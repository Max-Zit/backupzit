package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

type release struct {
	key  ed25519.PrivateKey
	keys []string
	raw  []byte
	sig  string
	pkg  []byte
	name string
}

func newRelease(t *testing.T, version string) *release {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	pkg := []byte("fake package " + version)
	sum := sha256.Sum256(pkg)
	name := "backupzit-server_" + version + "_amd64.deb"
	m := Manifest{Product: "backupzit", Version: version, Files: []File{{Name: name, Kind: "server", Format: "deb", Arch: "amd64",
		Size: int64(len(pkg)), SHA256: hex.EncodeToString(sum[:])}}}
	raw, _ := json.Marshal(m)
	return &release{key: key, keys: []string{base64.StdEncoding.EncodeToString(pub)}, raw: raw, sig: Sign(raw, key), pkg: pkg, name: name}
}

func TestVerify(t *testing.T) {
	r := newRelease(t, "0.28.0")
	m, err := Verify(r.raw, r.sig, r.keys)
	if err != nil || m.Version != "0.28.0" {
		t.Fatal(m, err)
	}
	if _, err := Verify(r.raw, r.sig, nil); err == nil {
		t.Error("release signed with an unknown key accepted by the built-in keys")
	}
	bad := append([]byte(nil), r.raw...)
	bad[len(bad)-3] ^= 1
	if _, err := Verify(bad, r.sig, r.keys); err == nil {
		t.Error("changed manifest accepted")
	}
	if !Newer("0.28.0", "0.27.9") || Newer("0.27.0", "0.27.0-legacy") || Newer("0.9.0", "0.10.0") || !Newer("1.0.0", "0.99.99") {
		t.Error("version comparison")
	}
	// A file source in a folder.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "manifest.json"), r.raw, 0o644)
	os.WriteFile(filepath.Join(dir, "manifest.json.sig"), []byte(r.sig+"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, r.name), r.pkg, 0o644)
	saved := TrustedKeys
	TrustedKeys = r.keys
	defer func() { TrustedKeys = saved }()
	src := Source{Base: "file://" + dir}
	got, _, _, err := src.Latest(context.Background())
	if err != nil || got.Version != "0.28.0" {
		t.Fatal(got, err)
	}
	dst := filepath.Join(t.TempDir(), r.name)
	if err := src.Download(context.Background(), got.Files[0], dst); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, r.name), []byte("tampered"), 0o644)
	if err := src.Download(context.Background(), got.Files[0], dst+"2"); err == nil {
		t.Error("tampered package downloaded")
	}
}

func setupApply(t *testing.T, r *release, healthOK *atomic.Bool) (*Applier, *[]string) {
	dir, work := t.TempDir(), t.TempDir()
	bin := filepath.Join(t.TempDir(), "backupzit-server")
	os.WriteFile(bin, []byte("old program"), 0o755)
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !healthOK.Load() {
			http.Error(w, "down", 503)
		}
	}))
	t.Cleanup(health.Close)
	var calls []string
	a := &Applier{Dir: dir, Work: work, Binary: bin, CurrentVersion: "0.27.0", DBURL: "postgres://x", HealthURL: health.URL,
		Service: "backupzit-server", Keys: r.keys, HealthWait: 3e9,
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			calls = append(calls, name+" "+strings.Join(args, " "))
			if name == "dpkg" {
				os.WriteFile(bin, []byte("new program"), 0o755)
			}
			return nil, nil
		}}
	os.WriteFile(filepath.Join(dir, r.name), r.pkg, 0o640)
	WriteJSON(filepath.Join(dir, "request.json"), Request{Manifest: r.raw, Signature: r.sig, File: r.name}, 0o640)
	return a, &calls
}

func TestApply(t *testing.T) {
	ctx := context.Background()
	var up atomic.Bool
	up.Store(true)
	r := newRelease(t, "0.28.0")
	a, calls := setupApply(t, r, &up)
	res, err := a.Apply(ctx)
	if err != nil || res.Status != "installed" || res.Version != "0.28.0" {
		t.Fatalf("%+v %v", res, err)
	}
	joined := strings.Join(*calls, "\n")
	if !strings.Contains(joined, "pg_dump --format=custom") || !strings.Contains(joined, "dpkg -i "+filepath.Join(a.Work, r.name)) ||
		!strings.Contains(joined, "systemctl restart backupzit-server") {
		t.Errorf("commands:\n%s", joined)
	}
	if _, err := os.Stat(filepath.Join(a.Dir, "request.json")); err == nil {
		t.Error("request not consumed")
	}
	var saved Result
	if ReadJSON(filepath.Join(a.Dir, "result.json"), &saved); saved.Status != "installed" {
		t.Errorf("result.json %+v", saved)
	}

	// The new version does not start: program and database come back.
	up.Store(false)
	a, calls = setupApply(t, r, &up)
	res, _ = a.Apply(ctx)
	if res.Status != "rolled-back" {
		t.Fatalf("%+v", res)
	}
	if b, _ := os.ReadFile(a.Binary); string(b) != "old program" {
		t.Errorf("program not restored: %q", b)
	}
	if joined := strings.Join(*calls, "\n"); !strings.Contains(joined, "pg_restore --clean") {
		t.Errorf("database not restored:\n%s", joined)
	}

	// Not signed by a trusted key, a downgrade, a swapped package: nothing runs.
	for name, mutate := range map[string]func(a *Applier, r *release){
		"untrusted": func(a *Applier, r *release) { a.Keys = []string{base64.StdEncoding.EncodeToString(make([]byte, 32))} },
		"downgrade": func(a *Applier, r *release) { a.CurrentVersion = "0.30.0" },
		"swapped":   func(a *Applier, r *release) { os.WriteFile(filepath.Join(a.Dir, r.name), []byte("evil"), 0o640) },
		"symlink": func(a *Applier, r *release) {
			p := filepath.Join(a.Dir, r.name)
			os.Remove(p)
			if os.Symlink(filepath.Join(t.TempDir(), "x"), p) != nil {
				os.WriteFile(p, []byte("evil"), 0o640) // no symlink rights on Windows
			}
		},
	} {
		up.Store(true)
		a, calls := setupApply(t, r, &up)
		mutate(a, r)
		res, _ := a.Apply(ctx)
		if res.Status != "failed" || len(*calls) != 0 {
			t.Errorf("%s: %+v, commands %v", name, res, *calls)
		}
	}
	// No request: nothing to do.
	a, _ = setupApply(t, r, &up)
	os.Remove(filepath.Join(a.Dir, "request.json"))
	if res, err := a.Apply(ctx); res != nil || err != nil {
		t.Errorf("without request: %v %v", res, err)
	}
}
