package update

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPGConnection(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct{ in, pass string }{
		{"postgres://backupzit:s3cr%3At@localhost:5432/backupzit?sslmode=disable", `s3cr\:t`},
		{"postgresql://backupzit@db/backupzit?password=p%5Cw&sslmode=disable", `p\\w`},
		{"postgres://backupzit@localhost/backupzit", ""},
		{"host=localhost user=backupzit dbname=backupzit", ""},
	} {
		conn, remove, err := pgConnection(tc.in, dir)
		if err != nil {
			t.Fatal(tc.in, err)
		}
		if tc.pass == "" {
			if conn != tc.in {
				t.Errorf("%s: changed to %s", tc.in, conn)
			}
			remove()
			continue
		}
		u, err := url.Parse(conn)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := u.User.Password(); ok || u.Query().Get("password") != "" || u.Query().Get("sslmode") != "disable" {
			t.Errorf("%s: %s", tc.in, conn)
		}
		pf := u.Query().Get("passfile")
		b, err := os.ReadFile(pf)
		if err != nil || string(b) != "*:*:*:*:"+tc.pass+"\n" {
			t.Errorf("%s: password file %q %v", tc.in, b, err)
		}
		if fi, err := os.Stat(pf); err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) {
			t.Errorf("%s: password file mode %v %v", tc.in, fi, err)
		}
		remove()
		if _, err := os.Stat(pf); err == nil {
			t.Errorf("%s: password file left behind", tc.in)
		}
	}
}

// The database password reaches pg_dump and pg_restore only through the
// password file, never on their command lines.
func TestApplyPasswordNotInArgs(t *testing.T) {
	r := newRelease(t, "0.28.0")
	for _, healthy := range []bool{true, false} {
		var up atomic.Bool
		up.Store(healthy)
		a, calls := setupApply(t, r, &up)
		a.DBURL = "postgres://backupzit:Secret123@localhost:5432/backupzit?sslmode=disable"
		run := a.Run
		a.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "pg_dump" || name == "pg_restore" {
				var conn string
				for _, arg := range args {
					if strings.HasPrefix(arg, "--dbname=") {
						conn = strings.TrimPrefix(arg, "--dbname=")
					}
				}
				u, err := url.Parse(conn)
				if err != nil {
					t.Fatal(err)
				}
				if b, err := os.ReadFile(u.Query().Get("passfile")); err != nil || string(b) != "*:*:*:*:Secret123\n" {
					t.Errorf("%s: password file %q %v", name, b, err)
				}
			}
			return run(ctx, name, args...)
		}
		res, _ := a.Apply(context.Background())
		joined := strings.Join(*calls, "\n")
		if strings.Contains(joined, "Secret123") || !strings.Contains(joined, "pg_dump") {
			t.Errorf("healthy=%v (%s): commands\n%s", healthy, res.Status, joined)
		}
		if !healthy && !strings.Contains(joined, "pg_restore") {
			t.Errorf("no rollback: %s", joined)
		}
		if left, _ := filepath.Glob(filepath.Join(a.Work, ".pgpass-*")); len(left) != 0 {
			t.Errorf("password file left behind: %v", left)
		}
	}
}
