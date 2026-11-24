package server_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/recovery"
	"github.com/max-zit/backupzit/internal/server"
)

// legacyFingerprint is the credentials fingerprint as access package
// records of PR #61 stored it: SHA-256 over the target's location and
// secrets, each followed by a zero byte, 16 hex characters. Records with it
// must keep comparing equal, so the format is pinned here independently.
func legacyFingerprint(t server.Target) string {
	h := sha256.New()
	for _, v := range []string{t.URL, t.SFTPPassword, t.SFTPKey, t.SFTPHostKey, t.S3AccessKey, t.S3SecretKey, t.S3Region,
		t.SMBPassword, t.SMBDomain, t.HardenedKey, t.HardenedFingerprint, t.AzureKey, t.AzureSAS, t.RecoveryKey} {
		h.Write([]byte(v))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// rawAccessSetting is the stored value of the list of access packages.
func rawAccessSetting(t *testing.T, e *env) string {
	t.Helper()
	var raw string
	if err := e.pool.QueryRow(e.ctx, `SELECT value::text FROM settings WHERE key='access_packages'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

// S1 of the review of PR #61: the fingerprints of storage credentials are
// unsalted hashes, so the list of access packages is stored sealed with
// the secrets key, never in the clear. A list an earlier build stored in
// the clear keeps working (its fingerprints still compare) and is sealed
// the first time it is read.
func TestAccessPackageRecordsSealed(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	tid, err := e.store.CreateTarget(ctx, server.Target{Name: "nas", Kind: "local", URL: t.TempDir(), Encrypted: true})
	if err != nil {
		t.Fatal(err)
	}
	tg, err := e.store.GetTarget(ctx, tid)
	if err != nil {
		t.Fatal(err)
	}
	fp := legacyFingerprint(tg)
	legacy := fmt.Sprintf(`[{"created":"2026-11-20T10:00:00Z","user":"olduser","repos":1,"targets":{"%d":"%s"}},
		{"created":"2026-11-21T10:00:00Z","user":"olduser","repos":1,"targets":{"%d":"0000000000000000"}}]`, tid, fp, tid)
	if _, err := e.pool.Exec(ctx, `INSERT INTO settings(key, value) VALUES('access_packages', $1::jsonb)`, legacy); err != nil {
		t.Fatal(err)
	}
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	for round := 0; round < 2; round++ { // as stored by the old build, then sealed
		_, _, body := admin.do("GET", "/recovery", nil)
		body = html.UnescapeString(body)
		if !strings.Contains(body, "Access packages created so far:") || !strings.Contains(body, "olduser") {
			t.Fatalf("round %d: list of packages not shown", round)
		}
		// Only the record whose fingerprint no longer matches is stale.
		if n := strings.Count(body, "no longer matches"); n != 1 {
			t.Errorf("round %d: %d stale warnings, want 1", round, n)
		}
		raw := rawAccessSetting(t, e)
		if !strings.HasPrefix(raw, `"enc:v1:`) || strings.Contains(raw, fp) || strings.Contains(raw, "olduser") {
			t.Fatalf("round %d: list stored in the clear: %s", round, raw)
		}
	}
}

// The console's access package form: it offers a generated passphrase,
// refuses weak ones and the console password with translated texts, and
// a package it creates opens with the passphrase. The list it keeps is
// sealed.
func TestAccessPackageCreate(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	const consolePass = "Kdjf8ehtn3bx-Rovinj"
	if err := e.store.SetPassword(ctx, "admin", consolePass); err != nil {
		t.Fatal(err)
	}
	repoURL := filepath.Join(t.TempDir(), "pc1")
	tid, err := e.store.CreateTarget(ctx, server.Target{Name: "nas", Kind: "local", URL: filepath.Dir(repoURL), Encrypted: true})
	if err != nil {
		t.Fatal(err)
	}
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	if _, err := e.store.EnrollAgent(ctx, api.EnrollRequest{Token: token, Hostname: "pc1", OS: "windows", Arch: "amd64", Version: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO runs(agent_id, kind, trigger, status, repo_url, target_id, snapshot_id, started_at, finished_at)
		SELECT id, 'image-backup', 'manual', 'success', $1, $2, 'abcd', now(), now() FROM agents LIMIT 1`, repoURL, tid); err != nil {
		t.Fatal(err)
	}
	admin := newClient(t, e)
	if !admin.login("admin", consolePass) {
		t.Fatal("login")
	}
	_, _, body := admin.do("GET", "/recovery", nil)
	if !strings.Contains(body, `id="accesspass"`) || !strings.Contains(body, `data-copy="#accesspass"`) {
		t.Fatal("no generated passphrase offered")
	}

	post := func(pass, lang string) (int, http.Header, string) {
		t.Helper()
		form := url.Values{"repo": {fmt.Sprintf("%d|%s", tid, repoURL)}, "passphrase": {pass}, "passphrase2": {pass}, "reauth_password": {consolePass}}
		req, _ := http.NewRequest("POST", e.ts.URL+"/recovery/access-package", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", e.ts.URL)
		if lang != "" {
			req.AddCookie(&http.Cookie{Name: "bz_lang", Value: lang})
		}
		resp, err := admin.c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.Header.Get("Content-Disposition") != "" {
			return resp.StatusCode, resp.Header, string(b)
		}
		return resp.StatusCode, resp.Header, html.UnescapeString(string(b))
	}
	for pass, want := range map[string]string{
		"aaaaaaaaaaaaaaaaaaaaaaaa": "The passphrase is too predictable",
		"Password2026!":            "The passphrase is a common password",
		consolePass:                "The passphrase must not be your console password.",
		"kdjfuehtnvbx":             "The passphrase needs more kinds of characters or more length.",
	} {
		_, h, body := post(pass, "")
		if h.Get("Content-Disposition") != "" || !strings.Contains(body, want) {
			t.Errorf("%q: download %q, page lacks %q", pass, h.Get("Content-Disposition"), want)
		}
	}
	// The texts are translated.
	if _, _, body := post("aaaaaaaaaaaaaaaaaaaaaaaa", "de"); !strings.Contains(body, "Die Passphrase ist zu leicht vorherzusagen") {
		t.Error("refusal not translated")
	}

	const pass = "Zgrada na Dorćolu 1934"
	code, h, body := post(pass, "")
	if code != 200 || !strings.Contains(h.Get("Content-Disposition"), recovery.AccessPackageExt) {
		t.Fatalf("create: %d %q", code, h.Get("Content-Disposition"))
	}
	p, err := recovery.OpenAccessPackage([]byte(body), pass)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Repositories) != 1 || p.Repositories[0].Location != repoURL || p.Repositories[0].Password == "" {
		t.Errorf("package %+v", p.Repositories)
	}
	if raw := rawAccessSetting(t, e); !strings.HasPrefix(raw, `"enc:v1:`) || strings.Contains(raw, "admin") {
		t.Errorf("list stored in the clear: %s", raw)
	}
	if code, _, _ := admin.do("POST", "/recovery/access-packages/forget", url.Values{}); code != http.StatusSeeOther {
		t.Errorf("forget: %d", code)
	}
	if raw := rawAccessSetting(t, e); !strings.HasPrefix(raw, `"enc:v1:`) {
		t.Errorf("empty list stored in the clear: %s", raw)
	}
	if _, _, body := admin.do("GET", "/recovery", nil); strings.Contains(body, "Access packages created so far:") {
		t.Error("forgotten list still shown")
	}
}
