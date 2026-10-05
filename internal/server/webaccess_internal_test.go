package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLimitAccess(t *testing.T) {
	s := &Server{guard: newLoginGuard(nil, time.Now)}
	s.access.v, s.access.at = WebAccess{Allow: []string{"203.0.113.10", "10.0.0.0/24"}}, time.Now().Add(time.Hour)
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(299) })
	for _, c := range []struct {
		remote, path string
		want         int
	}{
		{"203.0.113.10:5000", "/login", 299},
		{"10.0.0.7:5000", "/jobs", 299},
		{"198.51.100.1:5000", "/login", 404},
		{"198.51.100.1:5000", "/api/v1/status", 404},
		{"198.51.100.1:5000", "/api/agent/poll", 299},
		{"198.51.100.1:5000", "/api/deploy/x/agent.msi", 299},
		{"127.0.0.1:5000", "/login", 299},
		{"[::1]:5000", "/login", 299},
	} {
		r := httptest.NewRequest("GET", c.path, nil)
		r.RemoteAddr = c.remote
		w := httptest.NewRecorder()
		s.limitAccess(ok, false).ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s %s: %d, want %d", c.remote, c.path, w.Code, c.want)
		}
	}
	// Agents only on the main port: without a web port the setting cannot
	// hide the console (nobody could reach it).
	s.access.v = WebAccess{AgentPortOnly: true}
	r := httptest.NewRequest("GET", "/login", nil)
	r.RemoteAddr = "198.51.100.1:5000"
	w := httptest.NewRecorder()
	s.limitAccess(ok, true).ServeHTTP(w, r)
	if w.Code != 299 {
		t.Errorf("console hidden without a web port: %d", w.Code)
	}
	if err := (&WebAccess{Allow: []string{"not-an-ip"}}).Validate(); err == nil {
		t.Error("invalid network accepted")
	}
}
