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
	_, err := targetTestMessage(l, "S3", "", fmt.Errorf("connect: s3: access bucket: RequestTimeTooSkewed (%w)", backend.ErrClockSkew))
	if err == nil || !strings.Contains(err.Error(), "turn on time synchronization on the console") || strings.Contains(err.Error(), "check the user name") {
		t.Fatalf("message: %v", err)
	}
}
