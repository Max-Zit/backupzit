package server

import (
	"strings"
	"testing"
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
