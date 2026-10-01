package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLoginLimiter(t *testing.T) {
	l := newLoginLimiter()
	now := time.Now()
	l.now = func() time.Time { return now }
	r := httptest.NewRequest("POST", "/login", nil)
	r.RemoteAddr = "10.0.0.9:5555"
	keys := loginKeys(r, "admin")
	for i := 0; i < 5; i++ {
		if l.Blocked(keys) > 0 {
			t.Fatalf("blocked after %d failures", i)
		}
		l.Fail(keys)
	}
	if l.Blocked(keys) == 0 {
		t.Fatal("not blocked after 5 failures")
	}
	// Another address may still try the same user (up to the user limit).
	r2 := httptest.NewRequest("POST", "/login", nil)
	r2.RemoteAddr = "10.0.0.10:5555"
	if l.Blocked(loginKeys(r2, "admin")) > 0 {
		t.Error("other address blocked too early")
	}
	now = now.Add(16 * time.Minute)
	if l.Blocked(keys) > 0 {
		t.Error("still blocked after the window")
	}
	l.Fail(keys)
	l.Success(keys)
	if len(l.fails) != 0 {
		t.Error("success did not reset")
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
