package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLoginGuard(t *testing.T) {
	now := time.Now()
	g := newLoginGuard(nil, func() time.Time { return now })
	req := func(addr string) *http.Request {
		r := httptest.NewRequest("POST", "/login", nil)
		r.RemoteAddr = addr
		return r
	}
	r := req("10.0.0.9:5555")
	// Without a database addresses are counted but not blocked; the
	// per-username limit works in memory.
	for i := 0; i < 20; i++ {
		if g.Blocked(r, "admin") > 0 {
			t.Fatalf("username throttled after %d failures", i)
		}
		g.Fail(req(fmt.Sprintf("10.0.%d.1:1", i)), "Admin ", "sign-ins")
	}
	if g.Blocked(r, "admin") == 0 {
		t.Fatal("username not throttled after 20 failures")
	}
	if g.Blocked(r, "other") > 0 {
		t.Error("other username throttled")
	}
	now = now.Add(16 * time.Minute)
	if g.Blocked(r, "admin") > 0 {
		t.Error("still throttled after the window")
	}
	g.Fail(r, "admin", "sign-ins")
	g.Success(r, "admin")
	if g.fails["ip:10.0.0.9"] != nil || g.fails[userKey("admin")] != nil {
		t.Errorf("success did not reset: %v", g.fails)
	}

	// Client address behind trusted proxies.
	g.cfg, g.cfgAt = defaultLoginProtection, time.Now()
	g.cfg.TrustedProxies = []string{"10.1.0.0/16", "::1"}
	p := req("10.1.2.3:443")
	p.Header.Set("X-Forwarded-For", "6.6.6.6, 203.0.113.7, 10.1.9.9")
	if ip := g.ClientIP(p); ip != "203.0.113.7" {
		t.Errorf("client behind proxies = %s", ip)
	}
	p.RemoteAddr = "198.51.100.1:443" // not a proxy: header ignored
	if ip := g.ClientIP(p); ip != "198.51.100.1" {
		t.Errorf("spoofed X-Forwarded-For used: %s", ip)
	}
	p.RemoteAddr = "[::ffff:10.1.2.3]:443"
	p.Header.Set("X-Forwarded-For", "garbage")
	if ip := g.ClientIP(p); ip != "10.1.2.3" {
		t.Errorf("bad header: %s", ip)
	}

	// Settings.
	x := defaultLoginProtection
	if x.Validate() != nil || x.blockFor(1) != 30*time.Minute || x.blockFor(3) != 2*time.Hour || x.blockFor(20) != maxBlock {
		t.Error("progressive blocks wrong")
	}
	x.Progressive = false
	if x.blockFor(5) != 30*time.Minute {
		t.Error("non-progressive block grows")
	}
	for _, bad := range []LoginProtection{
		{MaxAttempts: 2, WindowMinutes: 15, BlockMinutes: 30, MaxUserAttempts: 20},
		{MaxAttempts: 5, WindowMinutes: 15, BlockMinutes: 30, MaxUserAttempts: 4},
		{MaxAttempts: 5, WindowMinutes: 15, BlockMinutes: 30, MaxUserAttempts: 20, TrustedNetworks: []string{"10.0.0.0/33"}},
		{MaxAttempts: 5, WindowMinutes: 15, BlockMinutes: 99999, MaxUserAttempts: 20},
	} {
		if bad.Validate() == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if !inPrefixes("192.168.10.77", splitNetworks("10.0.0.1\n192.168.10.0/24")) || inPrefixes("192.168.11.1", []string{"192.168.10.0/24"}) {
		t.Error("network matching wrong")
	}
}

func TestFlashSignature(t *testing.T) {
	if validFlash("Your session expired, call +1 555", "") {
		t.Error("unsigned message accepted")
	}
	msg := "Storage target added."
	if !validFlash(msg, flashSig(msg)) || validFlash(msg+"!", flashSig(msg)) {
		t.Error("signature check wrong")
	}
	w := httptest.NewRecorder()
	redirectErr(w, httptest.NewRequest("POST", "/x", nil), "/runs/1/browse?part=1", errTest("bad"))
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/runs/1/browse?part=1&") || !strings.Contains(loc, "sig=") {
		t.Errorf("redirect %q", loc)
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }
