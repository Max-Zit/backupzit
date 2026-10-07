package server

import (
	"testing"
	"time"
)

func TestTOTP(t *testing.T) {
	// RFC 6238 SHA-1 test vectors (last 6 digits).
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" // "12345678901234567890"
	for ts, want := range map[int64]string{59: "287082", 1111111109: "081804", 1234567890: "005924", 2000000000: "279037"} {
		got, err := totpCode(secret, ts/totpStep)
		if err != nil || got != want {
			t.Errorf("t=%d: %s, want %s", ts, got, want)
		}
	}
	now := time.Unix(1234567890, 0)
	code, _ := totpCode(secret, now.Unix()/totpStep)
	step, ok := totpVerify(secret, code, now, 0)
	if !ok {
		t.Fatal("current code rejected")
	}
	if _, ok := totpVerify(secret, code, now, step); ok {
		t.Error("replayed code accepted")
	}
	if _, ok := totpVerify(secret, code, now.Add(2*time.Minute), 0); ok {
		t.Error("old code accepted")
	}
	if _, ok := totpVerify(secret, "000000", now, 0); ok && code != "000000" {
		t.Error("wrong code accepted")
	}
	id, ok := parseMFAToken(mfaToken(42, now.Add(time.Minute)), now)
	if !ok || id != 42 {
		t.Error("mfa token")
	}
	if _, ok := parseMFAToken(mfaToken(42, now.Add(-time.Second)), now); ok {
		t.Error("expired mfa token accepted")
	}
	if _, ok := parseMFAToken("43"+mfaToken(42, now.Add(time.Minute))[2:], now); ok {
		t.Error("tampered mfa token accepted")
	}
}

// TOTPCodeForTest lets the HTTP tests compute codes.
func TOTPCodeForTest(secret string, now time.Time) string {
	c, _ := totpCode(secret, now.Unix()/totpStep)
	return c
}

// SetClock replaces the server clock in tests.
func SetClock(s *Server, f func() time.Time) { s.clock = f }

// SetTargetTestTimeout shortens storage connection tests in tests.
func SetTargetTestTimeout(d time.Duration) { targetTestTimeout = d }
