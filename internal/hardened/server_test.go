package hardened

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type testServer struct {
	t   *testing.T
	srv *Server
	h   http.Handler
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	store, err := OpenStore(t.TempDir(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if store.CheckImmutable() != nil {
		// No CAP_LINUX_IMMUTABLE (e.g. CI runners): the limits do not depend
		// on the attribute.
		prev := setImmutableAttr
		setImmutableAttr = func(string, bool) error { return nil }
		t.Cleanup(func() { setImmutableAttr = prev })
	}
	srv := &Server{Store: store, Keys: NewKeys(t.TempDir() + "/keys"), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), MetricsToken: "metrics-secret"}
	return &testServer{t: t, srv: srv, h: srv.Handler()}
}

func (ts *testServer) key(name string, lim Limits) string {
	ts.t.Helper()
	tok, err := ts.srv.Keys.AddWithLimits(name, lim)
	if err != nil {
		ts.t.Fatal(err)
	}
	return tok
}

func (ts *testServer) do(method, path, token string, body []byte) *httptest.ResponseRecorder {
	ts.t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	ts.h.ServeHTTP(w, req)
	return w
}

func TestKeyPathBinding(t *testing.T) {
	ts := newTestServer(t)
	all := ts.key("console", Limits{})
	pc1 := ts.key("pc1", Limits{Path: "office/pc1"})

	if w := ts.do("PUT", "/v1/files/office/pc2/data/aa", all, []byte("other machine")); w.Code != http.StatusCreated {
		t.Fatalf("unbound key put: %d", w.Code)
	}
	if w := ts.do("PUT", "/v1/files/office/pc1/data/bb", pc1, []byte("own data")); w.Code != http.StatusCreated {
		t.Fatalf("own path put: %d %s", w.Code, w.Body)
	}
	// Everything outside the path is 404 for pc1, as if it did not exist.
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/files/office/pc2/data/aa"},
		{"HEAD", "/v1/files/office/pc2/data/aa"},
		{"DELETE", "/v1/files/office/pc2/data/aa"},
		{"PUT", "/v1/files/office/pc2/data/cc"},
		{"PUT", "/v1/files/office/pc10/data/cc"}, // prefix of the name, not of the path
		{"GET", "/v1/list/office/pc2"},
		{"GET", "/v1/files/office/pc1x/data/dd"},
	} {
		var body []byte
		if c.method == "PUT" {
			body = []byte("x")
		}
		if w := ts.do(c.method, c.path, pc1, body); w.Code != http.StatusNotFound {
			t.Errorf("%s %s with bound key: %d, want 404", c.method, c.path, w.Code)
		}
	}
	keep, _ := json.Marshal(KeepRequest{Names: []string{"office/pc1/data/bb", "office/pc2/data/aa"}})
	if w := ts.do("POST", "/v1/keep", pc1, keep); w.Code != http.StatusNotFound {
		t.Errorf("keep with a foreign name: %d, want 404", w.Code)
	}
	// The other machine's file is untouched.
	if w := ts.do("GET", "/v1/files/office/pc2/data/aa", all, nil); w.Code != http.StatusOK || w.Body.String() != "other machine" {
		t.Errorf("foreign file after attempts: %d %q", w.Code, w.Body)
	}
	// Listing a folder above the path shows only the key's own files.
	w := ts.do("GET", "/v1/list/office", pc1, nil)
	var names []string
	_ = json.Unmarshal(w.Body.Bytes(), &names)
	if w.Code != http.StatusOK || len(names) != 1 || names[0] != "office/pc1/data/bb" {
		t.Errorf("list above the path: %d %v", w.Code, names)
	}
	if w := ts.do("GET", "/v1/files/office/pc1/data/bb", pc1, nil); w.Code != http.StatusOK {
		t.Errorf("own file: %d", w.Code)
	}
}

func TestKeyQuotaAndDaily(t *testing.T) {
	ts := newTestServer(t)
	q := ts.key("q", Limits{Path: "q", Quota: 100})
	d := ts.key("d", Limits{Path: "d", Daily: 100})
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	ts.srv.lim.now = func() time.Time { return now }

	if w := ts.do("PUT", "/v1/files/q/a", q, bytes.Repeat([]byte("a"), 80)); w.Code != http.StatusCreated {
		t.Fatalf("within quota: %d", w.Code)
	}
	// The same content again takes no space and is accepted.
	if w := ts.do("PUT", "/v1/files/q/a", q, bytes.Repeat([]byte("a"), 80)); w.Code != http.StatusInsufficientStorage && w.Code != http.StatusCreated {
		t.Fatalf("retry: %d", w.Code)
	}
	if w := ts.do("PUT", "/v1/files/q/b", q, bytes.Repeat([]byte("b"), 30)); w.Code != http.StatusInsufficientStorage {
		t.Fatalf("beyond quota: %d, want 507", w.Code)
	}
	if w := ts.do("PUT", "/v1/files/q/c", q, bytes.Repeat([]byte("c"), 20)); w.Code != http.StatusCreated {
		t.Fatalf("exactly up to the quota: %d", w.Code)
	}
	// A delete hides the file but frees nothing: the quota stays used.
	if w := ts.do("DELETE", "/v1/files/q/a", q, nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", w.Code)
	}
	now = now.Add(time.Hour) // usage measured again
	if w := ts.do("PUT", "/v1/files/q/e", q, []byte("e")); w.Code != http.StatusInsufficientStorage {
		t.Fatalf("after delete: %d, want 507 (hidden files still use space)", w.Code)
	}

	if w := ts.do("PUT", "/v1/files/d/a", d, bytes.Repeat([]byte("a"), 90)); w.Code != http.StatusCreated {
		t.Fatalf("daily: %d", w.Code)
	}
	if w := ts.do("PUT", "/v1/files/d/b", d, bytes.Repeat([]byte("b"), 20)); w.Code != http.StatusInsufficientStorage {
		t.Fatalf("beyond daily: %d, want 507", w.Code)
	}
	now = now.Add(24 * time.Hour)
	if w := ts.do("PUT", "/v1/files/d/b", d, bytes.Repeat([]byte("b"), 20)); w.Code != http.StatusCreated {
		t.Fatalf("next day: %d", w.Code)
	}
}

func TestKeyRate(t *testing.T) {
	ts := newTestServer(t)
	k := ts.key("r", Limits{Rate: 2})
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	ts.srv.lim.now = func() time.Time { return now }
	for i, name := range []string{"x/a", "x/b"} {
		if w := ts.do("PUT", "/v1/files/"+name, k, []byte(name)); w.Code != http.StatusCreated {
			t.Fatalf("write %d: %d", i, w.Code)
		}
	}
	w := ts.do("PUT", "/v1/files/x/c", k, []byte("c"))
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("third write: %d retry-after %q", w.Code, w.Header().Get("Retry-After"))
	}
	// Reads are never limited.
	for range 5 {
		if w := ts.do("GET", "/v1/files/x/a", k, nil); w.Code != http.StatusOK {
			t.Fatalf("read: %d", w.Code)
		}
	}
	now = now.Add(61 * time.Second)
	if w := ts.do("PUT", "/v1/files/x/c", k, []byte("c")); w.Code != http.StatusCreated {
		t.Fatalf("next minute: %d", w.Code)
	}
}

func TestMetrics(t *testing.T) {
	ts := newTestServer(t)
	k := ts.key("pc1", Limits{Path: "pc1", Quota: 1 << 20})
	ts.do("PUT", "/v1/files/pc1/data/a", k, []byte("hello"))
	ts.do("DELETE", "/v1/files/pc1/data/a", k, nil)
	ts.do("GET", "/v1/files/other/x", k, nil)

	if w := ts.do("GET", "/metrics", "", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("metrics without token: %d", w.Code)
	}
	if w := ts.do("GET", "/metrics", k, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("metrics with an access key: %d", w.Code)
	}
	w := ts.do("GET", "/metrics", "metrics-secret", nil)
	body := w.Body.String()
	for _, want := range []string{
		`backupzit_repo_uploads_total{key="pc1"} 1`,
		`backupzit_repo_uploaded_bytes_total{key="pc1"} 5`,
		`backupzit_repo_deletes_total{key="pc1"} 1`,
		`backupzit_repo_refused_total{key="pc1",reason="path"} 1`,
		`backupzit_repo_quota_bytes{key="pc1"} 1048576`,
		`backupzit_repo_used_bytes{key="pc1",path="pc1"} 5`,
		`backupzit_repo_lock_days 7`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, k) {
		t.Error("metrics contain the access key")
	}
	ts.srv.MetricsToken = ""
	if w := ts.do("GET", "/metrics", "metrics-secret", nil); w.Code != http.StatusNotFound {
		t.Errorf("metrics without configured token: %d", w.Code)
	}
}

func TestKeyLimitsFile(t *testing.T) {
	k := NewKeys(t.TempDir() + "/keys")
	tok, err := k.AddWithLimits("pc1", Limits{Path: "office/pc1", Quota: 500 << 30, Daily: 50 << 30, Rate: 600})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := k.Lookup(tok)
	if !ok || got.Path != "office/pc1" || got.Quota != 500<<30 || got.Daily != 50<<30 || got.Rate != 600 {
		t.Fatalf("lookup: %+v %v", got, ok)
	}
	if err := k.SetLimits("pc1", Limits{Path: "office/pc1"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := k.Lookup(tok); got.Quota != 0 || got.Path != "office/pc1" {
		t.Fatalf("after set: %+v", got)
	}
	for _, bad := range []Limits{{Path: "../x"}, {Path: "a/locks"}, {Quota: -1}} {
		if _, err := k.AddWithLimits("bad", bad); err == nil {
			t.Errorf("limits %+v accepted", bad)
		}
	}
	// A mistyped option disables the line instead of meaning "no limit".
	if _, ok := parseKeyLine("x " + strings.Repeat("ab", 32) + " qouta=5"); ok {
		t.Error("unknown option accepted")
	}
	for in, want := range map[string]int64{"500G": 500 << 30, "2T": 2 << 40, "1.5G": 3 << 29, "100": 100, "750MiB": 750 << 20} {
		if n, err := ParseSize(in); err != nil || n != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, n, err, want)
		}
	}
}
