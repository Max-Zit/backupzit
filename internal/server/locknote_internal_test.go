package server

import (
	"errors"
	"strings"
	"testing"
)

// The connection test of an S3 target with immutability warns when the
// bucket does not lock new objects by itself for the target's period.
func TestLockNote(t *testing.T) {
	l := languages[0]
	for _, c := range []struct {
		n    lockNote
		want string
	}{
		{lockNote{}, ""},
		{lockNote{checked: true, mode: "COMPLIANCE", days: 30, lockDays: 30}, ""},
		{lockNote{checked: true, mode: "COMPLIANCE", days: 60, lockDays: 30}, ""},
		{lockNote{checked: true, lockDays: 30}, "no default Object Lock retention in compliance mode"},
		{lockNote{checked: true, mode: "GOVERNANCE", days: 30, lockDays: 30}, "no default Object Lock retention in compliance mode"},
		{lockNote{checked: true, mode: "COMPLIANCE", days: 7, lockDays: 30}, "(7 days) is shorter than the immutability period of the target (30 days)"},
		{lockNote{checked: true, err: errors.New("AccessDenied"), lockDays: 30}, "could not be checked (AccessDenied)"},
	} {
		got := c.n.text(l)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%+v: %q, want %q", c.n, got, c.want)
		}
	}
}
