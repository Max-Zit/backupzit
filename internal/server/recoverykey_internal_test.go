package server

import (
	"fmt"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/backend"
)

// A recovery key is accepted as written down and stored in its usual form.
func TestParseRecoveryKey(t *testing.T) {
	k := NewRecoveryKey()
	for _, in := range []string{k, " " + k + " ", strings.ToLower(k), strings.ReplaceAll(k, "-", ""), strings.ReplaceAll(k, "-", " ")} {
		if got, err := parseRecoveryKey(in); err != nil || got != k {
			t.Errorf("%q: %q %v, want %q", in, got, err, k)
		}
	}
	for _, in := range []string{"secret", k[:len(k)-1], k + "-AAAA"} {
		if _, err := parseRecoveryKey(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

// A connection test refused because of the console's clock says so.
func TestTargetTestClockHint(t *testing.T) {
	l := languageByCode("en")
	_, err := targetTestMessage(l, "S3", "", "", fmt.Errorf("connect: s3: access bucket: RequestTimeTooSkewed (%w)", backend.ErrClockSkew))
	if err == nil || !strings.Contains(err.Error(), "turn on time synchronization on the console") || strings.Contains(err.Error(), "check the user name") {
		t.Fatalf("message: %v", err)
	}
	// An agent's test (#63) reports the error as text; the hint is about
	// that computer's clock.
	_, err = targetTestMessage(l, "S3", "nas-site", "", fmt.Errorf("connect: s3: access bucket: RequestTimeTooSkewed (%v)", backend.ErrClockSkew))
	if err == nil || !strings.Contains(err.Error(), "connection test on the agent nas-site failed") || !strings.Contains(err.Error(), "the clock of the agent's computer") {
		t.Fatalf("agent message: %v", err)
	}
}

// #63: a connection test the console cannot complete points to testing
// from an agent; the same failure on an agent names the agent.
func TestTargetTestReachHint(t *testing.T) {
	l := languageByCode("en")
	timeout := fmt.Errorf("connect: dial tcp 10.0.0.20:445: i/o timeout")
	_, err := targetTestMessage(l, "NAS", "", "", timeout)
	if err == nil || !strings.Contains(err.Error(), "that the console can reach the storage") || !strings.Contains(err.Error(), "test it from an agent and mark it reachable by agents only") {
		t.Fatalf("console: %v", err)
	}
	_, err = targetTestMessage(l, "NAS", "site-pc", "", timeout)
	if err == nil || !strings.Contains(err.Error(), "on the agent site-pc failed") || !strings.Contains(err.Error(), "that the agent can reach the storage") || strings.Contains(err.Error(), "console") {
		t.Fatalf("agent: %v", err)
	}
	msg, err := targetTestMessage(l, "NAS", "site-pc", "connected, wrote, read and removed a test file", nil)
	if err != nil || msg != "NAS: connection test on the agent site-pc passed — connected, wrote, read and removed a test file" {
		t.Fatalf("passed: %q %v", msg, err)
	}
}
