package main

import (
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/localipc"
)

// A running service that is not enrolled is not reported as "not running".
func TestSummaryNotEnrolled(t *testing.T) {
	s := summarize(&localipc.Status{ServerURL: "https://backup:443", LastError: "certificate mismatch"}, nil, time.Now())
	if s.Headline != "This computer is not enrolled yet" || !strings.Contains(s.Detail, "certificate mismatch") {
		t.Fatalf("%+v", s)
	}
	if s := summarize(nil, nil, time.Now()); s.Headline != "Agent service is not running" {
		t.Fatalf("no service: %q", s.Headline)
	}
}

// The console's web address wins over the agent address; consoles before
// 0.61 send none, and only web addresses are opened (#62).
func TestConsoleAddress(t *testing.T) {
	for _, c := range []struct {
		st   *localipc.Status
		want string
	}{
		{nil, ""},
		{&localipc.Status{ServerURL: "https://bz:8443"}, "https://bz:8443"},
		{&localipc.Status{ServerURL: "https://bz:8443", ConsoleURL: "https://backup.example.com"}, "https://backup.example.com"},
		{&localipc.Status{ServerURL: "https://bz:8443", ConsoleURL: "file:///etc/passwd"}, "https://bz:8443"},
		{&localipc.Status{ServerURL: "javascript:alert(1)"}, ""},
	} {
		if got := consoleAddress(c.st); got != c.want {
			t.Errorf("%+v: %q, want %q", c.st, got, c.want)
		}
	}
}
