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
	"unicode/utf8"

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
		case p == "/users/u-ana/calendars":
			writeJSON(w, map[string]any{"value": []map[string]string{{"id": "c1", "name": "Calendar"}}})
		case p == "/users/u-ana/calendars/c1/events":
			if !strings.Contains(r.Header.Get("Prefer"), `outlook.timezone="UTC"`) {
				w.WriteHeader(400)
				return
			}
			writeJSON(w, map[string]any{"value": []map[string]any{{
				"id": "e1", "iCalUId": "040000008200E00074C5B7101A82E008", "subject": "Weekly sync, team",
				"body":                 map[string]string{"content": "Agenda:\nstatus"},
				"start":                map[string]string{"dateTime": "2026-10-05T08:00:00.0000000", "timeZone": "UTC"},
				"end":                  map[string]string{"dateTime": "2026-10-05T08:30:00.0000000", "timeZone": "UTC"},
				"lastModifiedDateTime": day, "location": map[string]string{"displayName": "Room 1"},
				"organizer": map[string]any{"emailAddress": map[string]string{"name": "Ana", "address": "ana@contoso.test"}},
				"attendees": []map[string]any{{"type": "required", "emailAddress": map[string]string{"name": "Marko", "address": "marko@contoso.test"}, "status": map[string]string{"response": "accepted"}}},
				"recurrence": map[string]any{"pattern": map[string]any{"type": "weekly", "interval": 1, "daysOfWeek": []string{"monday"}, "firstDayOfWeek": "monday"},
					"range": map[string]any{"type": "numbered", "numberOfOccurrences": 10}},
			}}})
		case p == "/users/u-ana/contacts":
			writeJSON(w, map[string]any{"value": []map[string]any{{"id": "k1", "displayName": "Marko Marković", "givenName": "Marko", "surname": "Marković",
				"emailAddresses": []map[string]string{{"address": "marko@contoso.test"}}, "mobilePhone": "+381 64 123", "lastModifiedDateTime": day}}})
		case p == "/users/u-ana/contactFolders":
			writeJSON(w, map[string]any{"value": []map[string]string{{"id": "cf1", "displayName": "Suppliers"}}})
		case p == "/users/u-ana/contactFolders/cf1/contacts":
			writeJSON(w, map[string]any{"value": []map[string]any{{"id": "k2", "displayName": "Oil Ltd", "companyName": "Oil Ltd", "lastModifiedDateTime": day}}})
		case p == "/users/u-ana/contactFolders/cf1/childFolders":
			writeJSON(w, map[string]any{"value": []any{}})
		case p == "/sites/getAllSites":
			writeJSON(w, map[string]any{"value": []map[string]string{
				{"id": "s1", "displayName": "Sales", "webUrl": "https://contoso.sharepoint.com/sites/sales"},
				{"id": "s9", "displayName": "Ana", "webUrl": "https://contoso-my.sharepoint.com/personal/ana_contoso_test"},
			}})
		case p == "/sites/contoso.sharepoint.com:/sites/sales:":
			writeJSON(w, map[string]string{"id": "s1", "displayName": "Sales", "webUrl": "https://contoso.sharepoint.com/sites/sales"})
		case p == "/sites/s1/drives":
			writeJSON(w, map[string]any{"value": []map[string]string{{"id": "sd1", "name": "Documents", "driveType": "documentLibrary"}}})
		case p == "/drives/sd1/items/root/children":
			writeJSON(w, map[string]any{"value": []map[string]any{{"id": "i-pol", "name": "policy.pdf", "size": 6, "file": map[string]any{}, "lastModifiedDateTime": day}}})
		case p == "/drives/sd1/items/i-pol/content":
			g.downloads.Add(1)
			w.Write([]byte("policy"))
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

// Calendars, contacts and SharePoint libraries are stored as .ics, .vcf
// and files; unchanged items are not stored again.
func TestBackupCalendarContactsSharePoint(t *testing.T) {
	srv := newFakeGraph(t)
	g := fakes[srv.URL]
	ctx := context.Background()
	r := newRepo(t)
	c := NewClient(Credentials{Tenant: "contoso.test", Client: "app", Secret: "s3cret"})
	res, err := Backup(ctx, r, c, Options{Calendar: true, SharePoint: true, Users: []string{"ana@contoso.test"}})
	if err != nil {
		t.Fatal(err)
	}
	sn := res.Snapshot
	if len(sn.Stats.Errors) != 0 || strings.Join(res.Sites, ",") != "https://contoso.sharepoint.com/sites/sales" {
		t.Fatalf("errors %v sites %v", sn.Stats.Errors, res.Sites)
	}
	ics, _ := readFile(t, r, sn, "ana@contoso.test/Calendar/Calendar/2026-10-05 0800 Weekly sync, team ["+idHash("e1")+"].ics")
	ics = strings.ReplaceAll(ics, "\r\n ", "")
	for _, want := range []string{"BEGIN:VEVENT", "UID:040000008200E00074C5B7101A82E008", "DTSTART:20261005T080000Z", "DTEND:20261005T083000Z",
		"RRULE:FREQ=WEEKLY;BYDAY=MO;WKST=MO;COUNT=10", `SUMMARY:Weekly sync\, team`, `DESCRIPTION:Agenda:\nstatus`, "LOCATION:Room 1",
		"ORGANIZER;CN=Ana:mailto:ana@contoso.test", "ATTENDEE;CN=Marko;ROLE=REQ-PARTICIPANT;PARTSTAT=ACCEPTED:mailto:marko@contoso.test", "\r\n"} {
		if !strings.Contains(ics, want) {
			t.Errorf("ics lacks %q:\n%s", want, ics)
		}
	}
	vcf, _ := readFile(t, r, sn, "ana@contoso.test/Contacts/Marko Marković ["+idHash("k1")+"].vcf")
	for _, want := range []string{"BEGIN:VCARD", "FN:Marko Marković", "N:Marković;Marko;;;", "EMAIL;TYPE=INTERNET,PREF:marko@contoso.test", "TEL;TYPE=CELL:+381 64 123"} {
		if !strings.Contains(vcf, want) {
			t.Errorf("vcf lacks %q:\n%s", want, vcf)
		}
	}
	if vcf, _ = readFile(t, r, sn, "ana@contoso.test/Contacts/Suppliers/Oil Ltd ["+idHash("k2")+"].vcf"); !strings.Contains(vcf, "ORG:Oil Ltd;") {
		t.Errorf("contact folder: %s", vcf)
	}
	if body, _ := readFile(t, r, sn, "SharePoint/Sales/Documents/policy.pdf"); body != "policy" {
		t.Errorf("SharePoint file %q", body)
	}
	tree, _ := r.LoadTree(ctx, sn.Tree)
	spt, _ := r.LoadTree(ctx, *tree.Find(SharePointDir).Subtree)
	if len(spt.Nodes) != 1 {
		t.Errorf("personal site not left out: %+v", spt.Nodes)
	}

	// Named sites; unchanged items are reused.
	g.downloads.Store(0)
	res2, err := Backup(ctx, r, c, Options{Calendar: true, SharePoint: true, Users: []string{"ana@contoso.test"},
		Sites: []string{"https://contoso.sharepoint.com/sites/sales", "https://contoso.sharepoint.com/sites/gone"}, Parent: sn})
	if err != nil {
		t.Fatal(err)
	}
	s2 := res2.Snapshot.Stats
	if g.downloads.Load() != 0 || s2.FilesSkipped != s2.Files || s2.Files != 4 {
		t.Errorf("incremental: %d downloads, %+v", g.downloads.Load(), s2)
	}
	if len(s2.Errors) != 1 || !strings.Contains(s2.Errors[0], "sites/gone") {
		t.Errorf("errors %v", s2.Errors)
	}

	// SharePoint only.
	res3, err := Backup(ctx, r, c, Options{SharePoint: true})
	if err != nil || len(res3.Accounts) != 0 || len(res3.Snapshot.Paths) != 1 {
		t.Fatalf("SharePoint only: %v %+v", err, res3)
	}
}

func TestRRule(t *testing.T) {
	var e Event
	json.Unmarshal([]byte(`{"recurrence":{"pattern":{"type":"relativeMonthly","interval":2,"daysOfWeek":["friday"],"index":"last"},"range":{"type":"endDate","endDate":"2027-01-31"}}}`), &e)
	if got := e.rrule(); got != "RRULE:FREQ=MONTHLY;BYDAY=FR;BYSETPOS=-1;INTERVAL=2;UNTIL=20270131T235959Z" {
		t.Errorf("rrule %q", got)
	}
	long := foldLines([]string{"DESCRIPTION:" + strings.Repeat("ž", 60)})
	for _, l := range strings.Split(strings.TrimSuffix(string(long), "\r\n"), "\r\n") {
		if len(l) > 75 || !utf8.ValidString(strings.TrimPrefix(l, " ")) {
			t.Errorf("bad folded line %q", l)
		}
	}
}

// TestLivePIM reads calendars, contacts and all SharePoint sites of a real
// tenant (read-only); see TestLive for the variables.
func TestLivePIM(t *testing.T) {
	cfg := os.Getenv("BACKUPZIT_TEST_M365")
	if cfg == "" {
		t.Skip("BACKUPZIT_TEST_M365 not set")
	}
	tenant, client, _ := strings.Cut(cfg, ":")
	ctx := context.Background()
	r := newRepo(t)
	c := NewClient(Credentials{Tenant: tenant, Client: client, Secret: os.Getenv("BACKUPZIT_TEST_M365_SECRET")})
	opts := Options{Calendar: true, SharePoint: true, Users: []string{os.Getenv("BACKUPZIT_TEST_M365_USER")}}
	res, err := Backup(ctx, r, c, opts)
	if err != nil {
		t.Fatal(err)
	}
	s := res.Snapshot.Stats
	t.Logf("sites %v; %d files, %d dirs, %.1f MB, errors %v", res.Sites, s.Files, s.Dirs, float64(s.Bytes)/1e6, s.Errors)
	var walk func(id repo.ID, at string, depth int)
	walk = func(id repo.ID, at string, depth int) {
		tr, _ := r.LoadTree(ctx, id)
		n := 0
		for _, x := range tr.Nodes {
			if x.Type == repo.NodeDir {
				if depth < 8 {
					walk(*x.Subtree, at+"/"+x.Name, depth+1)
				}
			} else {
				n++
			}
		}
		if n > 0 {
			t.Logf("%s: %d files", at, n)
		}
	}
	walk(res.Snapshot.Tree, "", 0)
	opts.Parent = res.Snapshot
	res2, err := Backup(ctx, r, c, opts)
	if err != nil {
		t.Fatal(err)
	}
	if s2 := res2.Snapshot.Stats; s2.FilesSkipped != s2.Files {
		t.Errorf("incremental: %+v", s2)
	}
}
