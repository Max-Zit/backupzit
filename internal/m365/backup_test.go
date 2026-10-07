package m365

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
)

// fakeGraph serves a tenant with one licensed account (ana) and one
// without mailbox or OneDrive (room).
type fakeGraph struct {
	mu        sync.Mutex
	mime      map[string]string // message id -> MIME
	modified  map[string]time.Time
	downloads atomic.Int64
	throttled atomic.Bool
}

func newFakeGraph(t *testing.T) *httptest.Server {
	t.Helper()
	g := &fakeGraph{
		mime: map[string]string{
			"m1": "Subject: Hello\r\n\r\nfirst message\r\n",
			"m2": "Subject: Report: Q3/Q4?\r\n\r\n" + strings.Repeat("report line\r\n", 50000),
			"m3": "Subject: Plan\r\n\r\nproject plan\r\n",
		},
		modified: map[string]time.Time{},
	}
	base := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	for i, id := range []string{"m1", "m2", "m3"} {
		g.modified[id] = base.Add(time.Duration(i) * time.Hour)
	}
	var srv *httptest.Server
	writeJSON := func(w http.ResponseWriter, v any) { json.NewEncoder(w).Encode(v) }
	msg := func(id, subj string, recv time.Time) map[string]any {
		g.mu.Lock()
		defer g.mu.Unlock()
		return map[string]any{"id": id, "subject": subj, "receivedDateTime": recv, "lastModifiedDateTime": g.modified[id]}
	}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if strings.HasSuffix(p, "/oauth2/v2.0/token") {
			r.ParseForm()
			if r.Form.Get("client_secret") != "s3cret" {
				w.WriteHeader(401)
				writeJSON(w, map[string]string{"error": "invalid_client", "error_description": "AADSTS7000215: Invalid client secret provided. Ensure the secret is the value. Trace ID: x Correlation ID: y"})
				return
			}
			// header.payload.signature with roles
			payload := `{"roles":["Mail.Read","Files.Read.All","User.Read.All"]}`
			tok := "e30." + strings.TrimRight(b64(payload), "=") + ".sig"
			writeJSON(w, map[string]any{"access_token": tok, "expires_in": 3600})
			return
		}
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(401)
			return
		}
		p = strings.TrimPrefix(p, "/v1.0")
		day := time.Date(2026, 9, 30, 7, 15, 0, 0, time.UTC)
		switch {
		case p == "/users":
			writeJSON(w, map[string]any{"value": []map[string]string{
				{"id": "u-room", "userPrincipalName": "room@contoso.test"},
				{"id": "u-ana", "userPrincipalName": "Ana@contoso.test"},
			}})
		case p == "/users/ana@contoso.test":
			writeJSON(w, map[string]string{"id": "u-ana", "userPrincipalName": "Ana@contoso.test"})
		case p == "/users/nobody@contoso.test":
			w.WriteHeader(404)
			writeJSON(w, map[string]any{"error": map[string]string{"code": "Request_ResourceNotFound", "message": "not found"}})
		case strings.HasPrefix(p, "/users/u-room/"):
			w.WriteHeader(404)
			writeJSON(w, map[string]any{"error": map[string]string{"code": "MailboxNotEnabledForRESTAPI", "message": "no mailbox"}})
		case p == "/users/u-ana/mailFolders":
			writeJSON(w, map[string]any{"value": []map[string]any{
				{"id": "f-inbox", "displayName": "Inbox", "childFolderCount": 1},
				{"id": "f-sent", "displayName": "Sent Items"},
			}})
		case p == "/users/u-ana/mailFolders/f-inbox/childFolders":
			writeJSON(w, map[string]any{"value": []map[string]any{{"id": "f-proj", "displayName": "Projects: 2026"}}})
		case p == "/users/u-ana/mailFolders/f-inbox/messages":
			if r.URL.Query().Get("page") == "" {
				// two pages; the first answer is throttled once
				if !g.throttled.Swap(true) {
					w.Header().Set("Retry-After", "1")
					w.WriteHeader(429)
					return
				}
				writeJSON(w, map[string]any{"value": []any{msg("m1", "Hello", day)},
					"@odata.nextLink": srv.URL + "/v1.0/users/u-ana/mailFolders/f-inbox/messages?page=2"})
				return
			}
			writeJSON(w, map[string]any{"value": []any{msg("m2", "Report: Q3/Q4?", day.Add(time.Hour))}})
		case p == "/users/u-ana/mailFolders/f-proj/messages":
			writeJSON(w, map[string]any{"value": []any{msg("m3", "Plan", day)}})
		case p == "/users/u-ana/mailFolders/f-sent/messages":
			writeJSON(w, map[string]any{"value": []any{}})
		case strings.HasPrefix(p, "/users/u-ana/messages/") && strings.HasSuffix(p, "/$value"):
			id := strings.TrimSuffix(strings.TrimPrefix(p, "/users/u-ana/messages/"), "/$value")
			g.downloads.Add(1)
			g.mu.Lock()
			body, ok := g.mime[id]
			g.mu.Unlock()
			if !ok {
				w.WriteHeader(404)
				return
			}
			w.Write([]byte(body))
		case p == "/users/u-ana/drive":
			writeJSON(w, map[string]string{"id": "d1"})
		case p == "/drives/d1/items/root/children":
			writeJSON(w, map[string]any{"value": []map[string]any{
				{"id": "i-docs", "name": "Documents", "folder": map[string]int{"childCount": 1}, "lastModifiedDateTime": day},
				{"id": "i-top", "name": "notes.txt", "size": 5, "file": map[string]any{}, "lastModifiedDateTime": day},
			}})
		case p == "/drives/d1/items/i-docs/children":
			writeJSON(w, map[string]any{"value": []map[string]any{
				{"id": "i-offer", "name": "offer.docx", "size": 9, "file": map[string]any{}, "lastModifiedDateTime": day},
			}})
		case p == "/drives/d1/items/i-top/content":
			g.downloads.Add(1)
			// Real Graph redirects to a pre-authenticated URL.
			http.Redirect(w, r, srv.URL+"/download/notes", http.StatusFound)
		case p == "/download/notes":
			w.Write([]byte("notes"))
		case p == "/drives/d1/items/i-offer/content":
			g.downloads.Add(1)
			w.Write([]byte("offer doc"))
		default:
			w.WriteHeader(404)
			writeJSON(w, map[string]any{"error": map[string]string{"code": "itemNotFound", "message": p}})
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func(old1, old2 string) func() {
		return func() { LoginURL, GraphURL = old1, old2 }
	}(LoginURL, GraphURL))
	LoginURL, GraphURL = srv.URL, srv.URL+"/v1.0"
	fakes[srv.URL] = g
	return srv
}

var fakes = map[string]*fakeGraph{}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func newRepo(t *testing.T) *repo.Repository {
	t.Helper()
	be, err := backend.OpenLocal(filepath.Join(t.TempDir(), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(context.Background(), be)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// readFile returns the content of a snapshot path.
func readFile(t *testing.T, r *repo.Repository, sn *repo.Snapshot, path string) (string, *repo.Node) {
	t.Helper()
	ctx := context.Background()
	tree, err := r.LoadTree(ctx, sn.Tree)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(path, "/")
	for i, name := range parts {
		n := tree.Find(name)
		if n == nil {
			var names []string
			for _, x := range tree.Nodes {
				names = append(names, x.Name)
			}
			t.Fatalf("%s: %q not found in %v", path, name, names)
		}
		if i == len(parts)-1 {
			var buf bytes.Buffer
			for _, id := range n.Content {
				b, err := r.LoadBlob(ctx, repo.DataBlob, id)
				if err != nil {
					t.Fatal(err)
				}
				buf.Write(b)
			}
			return buf.String(), n
		}
		if tree, err = r.LoadTree(ctx, *n.Subtree); err != nil {
			t.Fatal(err)
		}
	}
	return "", nil
}

func init() { retryWait = func(int, string) time.Duration { return 10 * time.Millisecond } }

func TestBackupIncremental(t *testing.T) {
	srv := newFakeGraph(t)
	g := fakes[srv.URL]
	ctx := context.Background()
	r := newRepo(t)
	c := NewClient(Credentials{Tenant: "contoso.test", Client: "app", Secret: "s3cret"})

	roles, err := c.Roles(ctx)
	if err != nil || strings.Join(roles, ",") != "Mail.Read,Files.Read.All,User.Read.All" {
		t.Fatalf("roles %v %v", roles, err)
	}
	res, err := Backup(ctx, r, c, Options{Mail: true, OneDrive: true, Hostname: "proxy", Tags: []string{"job:7"}})
	if err != nil {
		t.Fatal(err)
	}
	sn := res.Snapshot
	if strings.Join(res.Accounts, ",") != "Ana@contoso.test" || strings.Join(res.Skipped, ",") != "room@contoso.test" {
		t.Fatalf("accounts %v skipped %v", res.Accounts, res.Skipped)
	}
	if len(sn.Stats.Errors) != 0 {
		t.Fatalf("errors: %v", sn.Stats.Errors)
	}
	if n, _ := c.Throttled(); n != 1 {
		t.Errorf("throttled %d times, want 1", n)
	}
	if got := g.downloads.Load(); got != 5 {
		t.Fatalf("first backup downloaded %d items, want 5", got)
	}
	if sn.Stats.Files != 5 || sn.Stats.FilesNew != 5 {
		t.Fatalf("stats %+v", sn.Stats)
	}
	want1 := "2026-09-30 0715 Hello [" + idHash("m1") + "].eml"
	body, n := readFile(t, r, sn, "ana@contoso.test/Mail/Inbox/"+want1)
	if body != g.mime["m1"] || !n.ModTime.Equal(g.modified["m1"]) {
		t.Fatalf("m1: %q %v", body, n.ModTime)
	}
	body, _ = readFile(t, r, sn, "ana@contoso.test/Mail/Inbox/2026-09-30 0815 Report_ Q3_Q4_ ["+idHash("m2")+"].eml")
	if body != g.mime["m2"] {
		t.Fatal("m2 differs")
	}
	if body, _ = readFile(t, r, sn, "ana@contoso.test/Mail/Inbox/Projects_ 2026/2026-09-30 0715 Plan ["+idHash("m3")+"].eml"); body != g.mime["m3"] {
		t.Fatal("m3 differs")
	}
	if body, _ = readFile(t, r, sn, "ana@contoso.test/OneDrive/notes.txt"); body != "notes" {
		t.Fatalf("notes.txt %q", body)
	}
	if body, _ = readFile(t, r, sn, "ana@contoso.test/OneDrive/Documents/offer.docx"); body != "offer doc" {
		t.Fatalf("offer.docx %q", body)
	}
	readFile(t, r, sn, "ana@contoso.test/Mail/Sent Items")

	// Unchanged: nothing is downloaded again.
	g.downloads.Store(0)
	res2, err := Backup(ctx, r, c, Options{Mail: true, OneDrive: true, Hostname: "proxy", Parent: sn})
	if err != nil {
		t.Fatal(err)
	}
	if got := g.downloads.Load(); got != 0 || res2.Snapshot.Stats.FilesSkipped != 5 {
		t.Fatalf("second backup downloaded %d, stats %+v", got, res2.Snapshot.Stats)
	}
	if res2.Snapshot.Stats.Bytes != sn.Stats.Bytes {
		t.Fatalf("bytes %d != %d", res2.Snapshot.Stats.Bytes, sn.Stats.Bytes)
	}

	// A changed message (read, flagged, edited) is read again.
	g.mu.Lock()
	g.modified["m3"] = g.modified["m3"].Add(time.Minute)
	g.mime["m3"] = "Subject: Plan\r\n\r\nproject plan v2\r\n"
	g.mu.Unlock()
	g.downloads.Store(0)
	res3, err := Backup(ctx, r, c, Options{Mail: true, OneDrive: true, Hostname: "proxy", Parent: res2.Snapshot})
	if err != nil {
		t.Fatal(err)
	}
	if got := g.downloads.Load(); got != 1 || res3.Snapshot.Stats.FilesChanged != 1 {
		t.Fatalf("third backup downloaded %d, stats %+v", got, res3.Snapshot.Stats)
	}
	if body, _ = readFile(t, r, res3.Snapshot, "ana@contoso.test/Mail/Inbox/Projects_ 2026/2026-09-30 0715 Plan ["+idHash("m3")+"].eml"); !strings.Contains(body, "v2") {
		t.Fatal("m3 not updated")
	}
	// The earlier backup still has the old version.
	if body, _ = readFile(t, r, sn, "ana@contoso.test/Mail/Inbox/Projects_ 2026/2026-09-30 0715 Plan ["+idHash("m3")+"].eml"); strings.Contains(body, "v2") {
		t.Fatal("old backup changed")
	}
}

func idHash(id string) string {
	return messageName(Message{ID: id})[len("0001-01-01 0000 (no subject) [") : len("0001-01-01 0000 (no subject) [")+10]
}

func TestBackupNamedAccounts(t *testing.T) {
	newFakeGraph(t)
	ctx := context.Background()
	r := newRepo(t)
	c := NewClient(Credentials{Tenant: "contoso.test", Client: "app", Secret: "s3cret"})
	res, err := Backup(ctx, r, c, Options{Mail: true, Users: []string{"ana@contoso.test", "nobody@contoso.test"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Snapshot.Stats.Errors) != 1 || !strings.Contains(res.Snapshot.Stats.Errors[0], "nobody@contoso.test: no such account") {
		t.Fatalf("errors %v", res.Snapshot.Stats.Errors)
	}
	tree, _ := r.LoadTree(ctx, res.Snapshot.Tree)
	ana, _ := r.LoadTree(ctx, *tree.Find("ana@contoso.test").Subtree)
	if len(ana.Nodes) != 1 || ana.Nodes[0].Name != MailDir {
		t.Fatalf("only mail expected: %+v", ana.Nodes)
	}

	if _, err := Backup(ctx, r, c, Options{Mail: true, Users: []string{"nobody@contoso.test"}}); err == nil || !strings.Contains(err.Error(), "no such account") {
		t.Fatalf("err %v", err)
	}
}

func TestWrongSecret(t *testing.T) {
	newFakeGraph(t)
	c := NewClient(Credentials{Tenant: "contoso.test", Client: "app", Secret: "wrong"})
	_, err := c.Roles(context.Background())
	if err == nil || !strings.Contains(err.Error(), "AADSTS7000215") || strings.Contains(err.Error(), "Trace") {
		t.Fatalf("err %v", err)
	}
}

func TestCleanName(t *testing.T) {
	for in, want := range map[string]string{
		"Re: a/b\\c?":    "Re_ a_b_c_",
		"  spaced\tout ": "spaced out",
		"dots...":        "dots",
		"CON":            "CON_",
		"":               "_",
		"ok.txt":         "ok.txt",
	} {
		if got := cleanName(in, 80); got != want {
			t.Errorf("cleanName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := cleanName(strings.Repeat("ž", 100), 80); len([]rune(got)) != 80 {
		t.Errorf("not shortened: %d", len([]rune(got)))
	}
	used := map[string]bool{}
	if a, b := uniqueName(used, "x.eml"), uniqueName(used, "X.eml"); a != "x.eml" || b != "X (2).eml" {
		t.Errorf("unique %q %q", a, b)
	}
}

// TestLive reads a real tenant (read-only): BACKUPZIT_TEST_M365 =
// tenant:client, BACKUPZIT_TEST_M365_SECRET, BACKUPZIT_TEST_M365_USER.
func TestLive(t *testing.T) {
	cfg := os.Getenv("BACKUPZIT_TEST_M365")
	if cfg == "" {
		t.Skip("BACKUPZIT_TEST_M365 not set")
	}
	tenant, client, _ := strings.Cut(cfg, ":")
	ctx := context.Background()
	r := newRepo(t)
	c := NewClient(Credentials{Tenant: tenant, Client: client, Secret: os.Getenv("BACKUPZIT_TEST_M365_SECRET")})
	user := os.Getenv("BACKUPZIT_TEST_M365_USER")
	start := time.Now()
	res, err := Backup(ctx, r, c, Options{Mail: true, OneDrive: true, Users: []string{user}, Hostname: "live-test"})
	if err != nil {
		t.Fatal(err)
	}
	s := res.Snapshot.Stats
	t.Logf("full: %d files, %d dirs, %.1f MB, %d errors, %v", s.Files, s.Dirs, float64(s.Bytes)/1e6, len(s.Errors), time.Since(start).Round(time.Second))
	for i, e := range s.Errors {
		if i < 10 {
			t.Log(e)
		}
	}
	start = time.Now()
	res2, err := Backup(ctx, r, c, Options{Mail: true, OneDrive: true, Users: []string{user}, Hostname: "live-test", Parent: res.Snapshot})
	if err != nil {
		t.Fatal(err)
	}
	s2 := res2.Snapshot.Stats
	t.Logf("incremental: %d files, %d unchanged, %d new, %d changed, %.1f MB read, %v", s2.Files, s2.FilesSkipped, s2.FilesNew, s2.FilesChanged, float64(s2.BytesRead)/1e6, time.Since(start).Round(time.Second))
	if s2.FilesSkipped < s.Files*9/10 {
		t.Errorf("incremental read too much: %+v", s2)
	}
}
