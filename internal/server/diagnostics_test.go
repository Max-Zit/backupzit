package server_test

import (
	"archive/zip"
	"bytes"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/server"
)

func TestDiagnostics(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	e.srv.LogFile = filepath.Join(t.TempDir(), "server.log")
	os.WriteFile(e.srv.LogFile, []byte("time=x level=INFO msg=\"server started\"\n"), 0o600)
	if _, err := e.store.CreateTarget(ctx, server.Target{Name: "nas", Kind: "sftp", URL: "sftp://bk@nas.example:22/repos", SFTPPassword: "Very-Secret-PW", SFTPHostKey: "SHA256:GZOluQp7ySvSWpb0rIObH+JVhyjCqp282yujoUhAvY0"}); err != nil {
		t.Fatal(err)
	}

	// A6: a user name typed at a failed sign-in must not become a formula
	// in the spreadsheet (anyone can type it, without an account).
	evil := newClient(t, e)
	evil.do("POST", "/login", url.Values{"username": {`=HYPERLINK("http://x.example","open")`}, "password": {"nope-nope-1"}})

	if _, _, body := admin.do("GET", "/settings/diagnostics", nil); !strings.Contains(body, "Download diagnostics") || !strings.Contains(body, "server started") {
		t.Error("diagnostics page lacks the download or the log")
	}
	code, _, body := admin.do("GET", "/settings/diagnostics/bundle", nil)
	if code != 200 {
		t.Fatalf("bundle: %d", code)
	}
	zr, err := zip.NewReader(bytes.NewReader([]byte(body)), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		files[f.Name] = string(b)
		if strings.Contains(string(b), "Very-Secret-PW") {
			t.Errorf("%s holds the storage password", f.Name)
		}
	}
	for _, name := range []string{"info.txt", "server.log", "agents.csv", "jobs.csv", "problems.csv", "audit.csv"} {
		if _, ok := files[name]; !ok {
			t.Errorf("zip lacks %s", name)
		}
	}
	if !strings.Contains(files["info.txt"], "BackupZit console") || !strings.Contains(files["info.txt"], "Storage targets: 1\n") || !strings.Contains(files["server.log"], "server started") || !strings.Contains(files["audit.csv"], "login") {
		t.Errorf("zip contents: %q", files["info.txt"])
	}
	if !strings.Contains(files["audit.csv"], "'=HYPERLINK") {
		t.Errorf("audit.csv: the failed user name is missing or not neutralized:\n%s", files["audit.csv"])
	}
	for _, l := range strings.Split(files["audit.csv"], "\n") {
		if strings.Contains(l, ",=HYPERLINK") || strings.Contains(l, ",\"=HYPERLINK") {
			t.Errorf("formula in audit.csv: %s", l)
		}
	}
}
