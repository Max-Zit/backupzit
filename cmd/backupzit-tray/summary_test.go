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
