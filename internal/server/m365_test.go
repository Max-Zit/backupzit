package server_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/m365"
	"github.com/max-zit/backupzit/internal/server"
	"github.com/max-zit/backupzit/internal/testutil"
)

// fakeTenant is a Microsoft 365 tenant with one mailbox (ana) holding two
// messages and a OneDrive with one file. roles are the granted permissions.
func fakeTenant(t *testing.T, roles ...string) *atomic.Int64 {
	t.Helper()
	var downloads atomic.Int64
	day := time.Date(2026, 10, 6, 9, 30, 0, 0, time.UTC)
	j := func(w http.ResponseWriter, v any) { json.NewEncoder(w).Encode(v) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/v1.0")
		switch {
		case strings.HasSuffix(p, "/oauth2/v2.0/token"):
			r.ParseForm()
			if r.Form.Get("client_secret") != "app-secret-value" {
				w.WriteHeader(401)
				j(w, map[string]string{"error": "invalid_client", "error_description": "AADSTS7000215: Invalid client secret provided."})
				return
			}
			claims, _ := json.Marshal(map[string]any{"roles": roles})
			j(w, map[string]any{"access_token": "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".sig", "expires_in": 3600})
		case p == "/users":
			j(w, map[string]any{"value": []map[string]string{{"id": "u1", "userPrincipalName": "ana@contoso.test"}}})
		case p == "/users/ana@contoso.test":
			j(w, map[string]string{"id": "u1", "userPrincipalName": "ana@contoso.test"})
		case p == "/users/u1/calendars":
			j(w, map[string]any{"value": []map[string]string{{"id": "c1", "name": "Calendar"}}})
		case p == "/users/u1/calendars/c1/events":
			j(w, map[string]any{"value": []map[string]any{{"id": "e1", "subject": "Board meeting", "lastModifiedDateTime": day,
				"start": map[string]string{"dateTime": "2026-10-08T10:00:00.0000000"}, "end": map[string]string{"dateTime": "2026-10-08T11:00:00.0000000"}}}})
		case p == "/users/u1/contacts":
			j(w, map[string]any{"value": []map[string]any{{"id": "k1", "displayName": "Marko", "lastModifiedDateTime": day}}})
		case p == "/users/u1/contactFolders":
			j(w, map[string]any{"value": []any{}})
		case p == "/sites/getAllSites":
			j(w, map[string]any{"value": []map[string]string{{"id": "s1", "displayName": "Sales", "webUrl": "https://contoso.sharepoint.com/sites/sales"}}})
		case p == "/sites/contoso.sharepoint.com:/sites/sales:":
			j(w, map[string]string{"id": "s1", "displayName": "Sales", "webUrl": "https://contoso.sharepoint.com/sites/sales"})
		case p == "/sites/s1/drives":
			j(w, map[string]any{"value": []map[string]string{{"id": "sd1", "name": "Documents"}}})
		case p == "/drives/sd1/items/root/children":
			j(w, map[string]any{"value": []map[string]any{{"id": "p1", "name": "price list.xlsx", "size": 11, "file": map[string]any{}, "lastModifiedDateTime": day}}})
		case p == "/drives/sd1/items/p1/content":
			downloads.Add(1)
			w.Write([]byte("price bytes"))
		case strings.HasPrefix(p, "/users/") && !strings.HasPrefix(p, "/users/u1"):
			w.WriteHeader(404)
			j(w, map[string]any{"error": map[string]string{"code": "Request_ResourceNotFound", "message": "not found"}})
		case p == "/users/u1/mailFolders":
			j(w, map[string]any{"value": []map[string]any{{"id": "inbox", "displayName": "Inbox"}}})
		case p == "/users/u1/mailFolders/inbox/messages":
			j(w, map[string]any{"value": []map[string]any{
				{"id": "a1", "subject": "Offer", "receivedDateTime": day, "lastModifiedDateTime": day},
				{"id": "a2", "subject": "Invoice 7/2026", "receivedDateTime": day.Add(time.Hour), "lastModifiedDateTime": day},
			}})
		case strings.HasPrefix(p, "/users/u1/messages/"):
			downloads.Add(1)
			id := strings.TrimSuffix(strings.TrimPrefix(p, "/users/u1/messages/"), "/$value")
			fmt.Fprintf(w, "Subject: message %s\r\n\r\nbody of %s\r\n", id, id)
		case p == "/users/u1/drive":
			j(w, map[string]string{"id": "d1"})
		case p == "/drives/d1/items/root/children":
			j(w, map[string]any{"value": []map[string]any{{"id": "f1", "name": "plan.xlsx", "size": 10, "file": map[string]any{}, "lastModifiedDateTime": day}}})
		case p == "/drives/d1/items/f1/content":
			downloads.Add(1)
			w.Write([]byte("plan bytes"))
		default:
			w.WriteHeader(404)
			j(w, map[string]any{"error": map[string]string{"code": "itemNotFound", "message": p}})
		}
	}))
	oldL, oldG := m365.LoginURL, m365.GraphURL
	m365.LoginURL, m365.GraphURL = srv.URL, srv.URL+"/v1.0"
	t.Cleanup(func() { m365.LoginURL, m365.GraphURL = oldL, oldG; srv.Close() })
	return &downloads
}

const (
	testTenant = "0244a840-1973-4553-901c-ead0f5823b22"
	testClient = "6be64ab9-0b80-44b4-a37e-5dd8fb5644db"
)

// A Microsoft 365 job checks the app when it is saved, stores the secret
// encrypted, hands the tenant to the agent, which backs up mail and
// OneDrive into the tenant's repository; the backup is browsed,
// downloaded and restored into a folder, never to an original location.
func TestM365Job(t *testing.T) {
	downloads := fakeTenant(t, "User.Read.All", "Mail.Read", "Files.Read.All")
	e := setup(t)
	ctx := e.ctx
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test")
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	ag.VSS = false
	agents, _ := e.store.ListAgents(ctx)
	s3, err := testutil.StartS3Server("backups")
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	target, err := e.store.CreateTarget(ctx, server.Target{Name: "s3", Kind: "s3", URL: "s3://" + s3.Host + "/backups/m365?tls=false", S3AccessKey: "AK", S3SecretKey: "SK"})
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"name": {"Office 365"}, "kind": {"m365"}, "agent_id": {fmt.Sprint(agents[0].ID)}, "target_id": {fmt.Sprint(target)},
		"m365_form": {"1"}, "m365_tenant": {testTenant}, "m365_client": {testClient}, "m365_secret": {"wrong"},
		"m365_mail": {"on"}, "m365_onedrive": {"on"}, "m365_users": {"Ana@contoso.test"}, "sched_kind": {"manual"}, "keep_last": {"5"}}
	for _, c := range []struct{ field, value, want string }{
		{"m365_tenant", "not a tenant", "directory (tenant) ID"},
		{"m365_client", "abc", "application (client) ID"},
		{"m365_users", "ana", "is not an account address"},
		{"m365_secret", "wrong", "client secret is wrong"},
		{"m365_users", "nobody@contoso.test", "does not exist in this tenant"},
	} {
		f := url.Values{}
		for k, v := range form {
			f[k] = v
		}
		f.Set("m365_secret", "app-secret-value")
		f.Set(c.field, c.value)
		_, loc, _ := admin.do("POST", "/jobs", f)
		msg, _ := url.QueryUnescape(loc)
		if !strings.Contains(msg, "err=") || !strings.Contains(msg, c.want) {
			t.Errorf("%s=%q: %s", c.field, c.value, msg)
		}
	}
	form.Set("m365_secret", "app-secret-value")
	_, loc, _ := admin.do("POST", "/jobs", form)
	if msg, _ := url.QueryUnescape(loc); !strings.Contains(msg, "msg=Job created. Microsoft 365 checked") {
		t.Fatalf("create: %s", msg)
	}
	var id int64
	fmt.Sscanf(strings.TrimPrefix(loc, "/jobs/"), "%d", &id)
	var stored string
	e.pool.QueryRow(ctx, `SELECT options->>'m365_secret' FROM jobs WHERE id=$1`, id).Scan(&stored)
	if stored == "" || strings.Contains(stored, "app-secret") {
		t.Errorf("secret stored as %q", stored)
	}
	_, _, body := admin.do("GET", fmt.Sprintf("/jobs/%d", id), nil)
	if strings.Contains(body, "app-secret") || !strings.Contains(body, "ana@contoso.test") || !strings.Contains(body, testTenant) {
		t.Error("job page shows the secret or lacks the account")
	}
	_, _, body = admin.do("GET", "/jobs", nil)
	if !strings.Contains(body, "Microsoft 365 Mail + OneDrive: ") {
		t.Error("job list lacks the Microsoft 365 job")
	}

	rid, err := e.store.QueueBackup(ctx, id, "manual")
	if err != nil {
		t.Fatal(err)
	}
	ar, err := e.srv.APIRun(ctx, rid)
	if err != nil {
		t.Fatal(err)
	}
	if ar.M365 == nil || ar.M365.Secret != "app-secret-value" || ar.M365.Tenant != testTenant || !ar.M365.Mail || !ar.M365.OneDrive ||
		strings.Join(ar.Paths, ",") != "ana@contoso.test" || !strings.Contains(ar.Repository.URL, "/m365/m365-"+testTenant+"?") {
		t.Fatalf("agent gets %+v %+v", ar, ar.M365)
	}
	runAgent(t, ag)
	run, _ := e.store.GetRun(ctx, rid)
	if run.Status != api.StatusSuccess || run.SnapshotID == "" || !strings.Contains(run.Message, "ana@contoso.test") {
		t.Fatalf("backup: %s %s %v", run.Status, run.Message, run.Errors)
	}
	if downloads.Load() != 3 {
		t.Errorf("downloaded %d items", downloads.Load())
	}
	// The second backup downloads nothing that did not change.
	rid2, _ := e.store.QueueBackup(ctx, id, "manual")
	runAgent(t, ag)
	if run2, _ := e.store.GetRun(ctx, rid2); run2.Status != api.StatusSuccess || downloads.Load() != 3 {
		t.Fatalf("second backup: %s %s, %d downloads", run2.Status, run2.Message, downloads.Load())
	}

	// Browse: account, Mail, Inbox, messages.
	_, _, body = admin.do("GET", fmt.Sprintf("/runs/%d/files?path=%s", rid, url.QueryEscape("ana@contoso.test/Mail/Inbox")), nil)
	if !strings.Contains(body, "2026-10-06 0930 Offer [") || !strings.Contains(body, "Invoice 7_2026") {
		t.Fatalf("browse lacks the messages: %s", body)
	}
	if strings.Contains(body, `value="original"`) {
		t.Error("original location offered for Microsoft 365")
	}
	_, _, body = admin.do("GET", fmt.Sprintf("/runs/%d/files?path=%s", rid, url.QueryEscape("ana@contoso.test/OneDrive")), nil)
	if !strings.Contains(body, "plan.xlsx") {
		t.Fatal("browse lacks the OneDrive file")
	}
	v := url.Values{"path": {"ana@contoso.test/OneDrive"}, "sel": {"ana@contoso.test/OneDrive/plan.xlsx"}}
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/runs/%d/files-download", e.ts.URL, rid), strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.ts.URL)
	resp, err := admin.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if b, _ := io.ReadAll(resp.Body); string(b) != "plan bytes" {
		t.Errorf("download %q", b)
	}

	// The restore wizard lists the backups under their own type.
	if _, _, p := admin.do("GET", "/restore", nil); !strings.Contains(p, `href="/restore?type=m365"`) {
		t.Error("wizard lacks Microsoft 365")
	}
	if _, _, p := admin.do("GET", fmt.Sprintf("/restore?type=m365&src=job:%d&run=%d", id, rid), nil); !strings.Contains(p, "Restore into a folder") || strings.Contains(p, `value="original"`) {
		t.Error("wizard restore form")
	}
	if _, _, p := admin.do("GET", "/restore?type=files", nil); strings.Contains(p, "Office 365") {
		t.Error("Microsoft 365 backups listed as files")
	}

	// Restore: only into a folder.
	if _, err := e.store.QueueRestore(ctx, rid, agents[0].ID, "", nil, false); err == nil || !strings.Contains(err.Error(), "restored into a folder") {
		t.Errorf("original location: %v", err)
	}
	dest := t.TempDir()
	if _, err := e.store.QueueRestore(ctx, rid, agents[0].ID, dest, nil, true); err != nil {
		t.Fatal(err)
	}
	runAgent(t, ag)
	var emls int
	var offer string
	filepath.Walk(dest, func(p string, fi os.FileInfo, err error) error {
		if err == nil && strings.HasSuffix(p, ".eml") {
			emls++
			if strings.Contains(p, "Offer") {
				b, _ := os.ReadFile(p)
				offer = string(b)
			}
		}
		return nil
	})
	if emls != 2 || !strings.Contains(offer, "body of a1") {
		t.Errorf("restored %d messages, offer %q", emls, offer)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "ana@contoso.test", "OneDrive", "plan.xlsx")); err != nil || string(b) != "plan bytes" {
		t.Errorf("restored OneDrive file %q %v", b, err)
	}

	// A copy job reads and writes the tenant's repository.
	second, _ := e.store.CreateTarget(ctx, server.Target{Name: "second", Kind: "local", URL: t.TempDir()})
	cid, err := e.store.CreateJob(ctx, server.Job{Kind: server.JobCopy, AgentID: agents[0].ID, TargetID: second, Name: "m365 copy", Enabled: true, SourceJobID: &id})
	if err != nil {
		t.Fatal(err)
	}
	crid, err := e.store.QueueBackup(ctx, cid, "manual")
	if err != nil {
		t.Fatal(err)
	}
	runAgent(t, ag)
	if cr, _ := e.store.GetRun(ctx, crid); cr.Status != api.StatusSuccess || !strings.HasSuffix(cr.RepoURL, "/m365-"+testTenant) {
		t.Errorf("copy: %s %s %s", cr.Status, cr.Message, cr.RepoURL)
	}

	// Editing without the secret keeps it.
	edit := url.Values{"name": {"Office 365"}, "m365_form": {"1"}, "m365_tenant": {testTenant}, "m365_client": {testClient},
		"m365_mail": {"on"}, "m365_users": {"ana@contoso.test"}, "sched_kind": {"manual"}, "keep_last": {"5"}}
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/jobs/%d/edit", id), edit); !strings.Contains(loc, "msg=") {
		msg, _ := url.QueryUnescape(loc)
		t.Fatalf("edit: %s", msg)
	}
	rid3, _ := e.store.QueueBackup(ctx, id, "manual")
	if ar, err := e.srv.APIRun(ctx, rid3); err != nil || ar.M365.Secret != "app-secret-value" || ar.M365.OneDrive {
		t.Errorf("after edit: %+v %v", ar.M365, err)
	}
}

// Missing application permissions are named when the job is saved.
func TestM365JobMissingPermissions(t *testing.T) {
	fakeTenant(t, "User.Read.All")
	e := setup(t)
	ctx := e.ctx
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	agent.Enroll(ctx, e.ts.URL, token, e.fp, "test")
	agents, _ := e.store.ListAgents(ctx)
	target, _ := e.store.CreateTarget(ctx, server.Target{Name: "store", Kind: "local", URL: t.TempDir()})
	form := url.Values{"name": {"m365"}, "kind": {"m365"}, "agent_id": {fmt.Sprint(agents[0].ID)}, "target_id": {fmt.Sprint(target)},
		"m365_form": {"1"}, "m365_tenant": {testTenant}, "m365_client": {testClient}, "m365_secret": {"app-secret-value"},
		"m365_mail": {"on"}, "m365_onedrive": {"on"}, "sched_kind": {"manual"}}
	_, loc, _ := admin.do("POST", "/jobs", form)
	msg, _ := url.QueryUnescape(loc)
	if !strings.Contains(msg, "missing the application permissions Mail.Read, Files.Read.All") {
		t.Fatalf("create: %s", msg)
	}
	// In Serbian.
	admin.do("POST", "/lang", url.Values{"lang": {"sr"}, "back": {"/jobs"}})
	_, loc, _ = admin.do("POST", "/jobs", form)
	msg, _ = url.QueryUnescape(loc)
	if !strings.Contains(msg, "Aplikaciji nedostaju aplikacione dozvole Mail.Read, Files.Read.All") {
		t.Fatalf("create (sr): %s", msg)
	}
}

// Calendars, contacts and SharePoint: the permissions and sites are
// checked when the job is saved; a SharePoint-only job needs no accounts.
func TestM365CalendarSharePointJob(t *testing.T) {
	fakeTenant(t, "User.Read.All", "Mail.Read", "Files.Read.All", "Calendars.Read", "Contacts.Read", "Sites.Read.All")
	e := setup(t)
	ctx := e.ctx
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test")
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	agents, _ := e.store.ListAgents(ctx)
	s3, err := testutil.StartS3Server("backups")
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	target, _ := e.store.CreateTarget(ctx, server.Target{Name: "s3", Kind: "s3", URL: "s3://" + s3.Host + "/backups/m365?tls=false", S3AccessKey: "AK", S3SecretKey: "SK"})
	form := url.Values{"name": {"sp"}, "kind": {"m365"}, "agent_id": {fmt.Sprint(agents[0].ID)}, "target_id": {fmt.Sprint(target)},
		"m365_form": {"1"}, "m365_tenant": {testTenant}, "m365_client": {testClient}, "m365_secret": {"app-secret-value"},
		"m365_calendar": {"on"}, "m365_sharepoint": {"on"}, "m365_users": {"ana@contoso.test"}, "sched_kind": {"manual"}}
	for site, want := range map[string]string{
		"http://contoso.sharepoint.com/sites/sales":  "is not a SharePoint site address",
		"https://contoso.sharepoint.com/sites/gone": "does not exist in this tenant",
	} {
		form.Set("m365_sites", site)
		_, loc, _ := admin.do("POST", "/jobs", form)
		if msg, _ := url.QueryUnescape(loc); !strings.Contains(msg, want) {
			t.Errorf("%s: %s", site, msg)
		}
	}
	form.Set("m365_sites", "https://contoso.sharepoint.com/sites/sales/")
	_, loc, _ := admin.do("POST", "/jobs", form)
	msg, _ := url.QueryUnescape(loc)
	if !strings.Contains(msg, "permissions User.Read.All, Calendars.Read, Contacts.Read, Sites.Read.All") {
		t.Fatalf("create: %s", msg)
	}
	var id int64
	fmt.Sscanf(strings.TrimPrefix(loc, "/jobs/"), "%d", &id)
	_, _, body := admin.do("GET", "/jobs", nil)
	if !strings.Contains(body, "Microsoft 365 Calendars and contacts + SharePoint: <span class=\"mono\">ana@contoso.test</span>; SharePoint <span class=\"mono\">https://contoso.sharepoint.com/sites/sales</span>") {
		t.Error("job list")
	}
	rid, _ := e.store.QueueBackup(ctx, id, "manual")
	ar, err := e.srv.APIRun(ctx, rid)
	if err != nil || ar.M365.Mail || ar.M365.OneDrive || !ar.M365.Calendar || !ar.M365.SharePoint || strings.Join(ar.M365.Sites, ",") != "https://contoso.sharepoint.com/sites/sales" {
		t.Fatalf("agent gets %+v %v", ar.M365, err)
	}
	runAgent(t, ag)
	run, _ := e.store.GetRun(ctx, rid)
	if run.Status != api.StatusSuccess || !strings.Contains(run.Message, "1 SharePoint site: https://contoso.sharepoint.com/sites/sales") {
		t.Fatalf("backup: %s %s %v", run.Status, run.Message, run.Errors)
	}
	for dir, want := range map[string]string{
		"ana@contoso.test/Calendar/Calendar": "2026-10-08 1000 Board meeting [",
		"ana@contoso.test/Contacts":          "Marko [",
		"SharePoint/Sales/Documents":         "price list.xlsx",
	} {
		_, _, body := admin.do("GET", fmt.Sprintf("/runs/%d/files?path=%s", rid, url.QueryEscape(dir)), nil)
		if !strings.Contains(body, want) {
			t.Errorf("%s lacks %q", dir, want)
		}
	}

	// SharePoint only: accounts are not needed; an old agent is refused.
	form.Del("m365_calendar")
	form.Set("m365_sites", "")
	_, loc, _ = admin.do("POST", "/jobs", form)
	msg, _ = url.QueryUnescape(loc)
	if !strings.Contains(msg, "the permissions Sites.Read.All.") {
		t.Fatalf("SharePoint only: %s", msg)
	}
	fmt.Sscanf(strings.TrimPrefix(loc, "/jobs/"), "%d", &id)
	if j, _ := e.store.GetJob(ctx, id); len(j.Paths) != 0 {
		t.Errorf("accounts kept: %v", j.Paths)
	}
	e.pool.Exec(ctx, `UPDATE agents SET version='0.34.0'`)
	rid, _ = e.store.QueueBackup(ctx, id, "manual")
	if _, err := e.srv.APIRun(ctx, rid); err == nil || !strings.Contains(err.Error(), "cannot back up Microsoft 365 SharePoint yet; update it to 0.35.0") {
		t.Errorf("old agent: %v", err)
	}
}
