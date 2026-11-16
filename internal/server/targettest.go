package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
)

// targetTestTimeout bounds a connection test (tests shorten it).
var targetTestTimeout = 30 * time.Second

// lockNote is what a connection test found about the default Object Lock
// retention of an S3 bucket with immutability on.
type lockNote struct {
	checked  bool
	err      error
	mode     string
	days     int
	lockDays int
}

// text is the warning for the page, "" when the bucket protects its
// objects by itself for at least the target's immutability period.
func (n lockNote) text(l *language) string {
	switch {
	case !n.checked:
		return ""
	case n.err != nil:
		return " " + l.T("The default retention of the bucket could not be checked (%v); make sure the bucket locks new objects in compliance mode for at least %d days by itself.", n.err, n.lockDays)
	case n.mode != "COMPLIANCE":
		return " " + l.T("Warning: the bucket has no default Object Lock retention in compliance mode, so the agents decide which objects are locked. Set a default retention of at least %d days in compliance mode on the bucket, so a compromised computer cannot write unlocked backups.", n.lockDays)
	case n.days < n.lockDays:
		return " " + l.T("Warning: the default retention of the bucket (%d days) is shorter than the immutability period of the target (%d days); set it to at least %d days.", n.days, n.lockDays, n.lockDays)
	}
	return ""
}

// testTargetLock checks that the console can reach a storage target with its
// credentials: it writes, reads back and removes a small file, or only
// lists on storage where files cannot be removed (S3 Object Lock,
// hardened repository); for an S3 bucket with immutability it also checks
// the bucket's default retention. Agents write the backups; the console
// tests from its own network.
func testTargetLock(ctx context.Context, t Target) (string, lockNote, error) {
	var note lockNote
	if t.Kind == "local" || t.Kind == "usb" {
		return "", note, errors.New("a local or USB disk is only reachable by its agent; the first backup tests it")
	}
	ctx, cancel := context.WithTimeout(ctx, targetTestTimeout)
	defer cancel()
	be, done, err := openTargetBackend(ctx, t, t.URL)
	if err != nil {
		return "", note, fmt.Errorf("connect: %w", err)
	}
	defer done()
	if t.S3LockDays > 0 || t.Kind == "hardened" {
		if _, err := be.List(ctx, "keys"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", note, fmt.Errorf("list: %w", err)
		}
		if t.S3LockDays > 0 {
			if mode, days, ok, err := backend.DefaultRetentionOf(ctx, be); ok {
				note = lockNote{checked: true, err: err, mode: mode, days: days, lockDays: t.S3LockDays}
			}
		}
		return "connected and signed in (immutable storage: nothing written)", note, nil
	}
	b := make([]byte, 8)
	rand.Read(b)
	name := "backupzit-test-" + hex.EncodeToString(b)
	data := []byte("BackupZit connection test " + time.Now().UTC().Format(time.RFC3339))
	if err := be.Save(ctx, name, data); err != nil {
		return "", note, fmt.Errorf("write a test file: %w", err)
	}
	got, err := be.Load(ctx, name)
	if err == nil && !bytes.Equal(got, data) {
		err = errors.New("the file read back differs")
	}
	if rerr := be.Remove(ctx, name); err == nil && rerr != nil {
		err = fmt.Errorf("remove the test file: %w", rerr)
	}
	if err != nil {
		return "", note, fmt.Errorf("read: %w", err)
	}
	return "connected, wrote, read and removed a test file", note, nil
}

// targetTestMessage words a test result for the page.
func targetTestMessage(l *language, name, ok string, err error) (string, error) {
	if err != nil {
		hint := ""
		switch m := strings.ToLower(err.Error()); {
		case errors.Is(err, backend.ErrClockSkew):
			hint = " " + l.T("(the clock of the console differs from the storage's: turn on time synchronization on the console)")
		case strings.Contains(m, "ssh_fx_failure") || strings.Contains(m, "no space") || strings.Contains(m, "quota"):
			hint = " " + l.T("(the storage may be full or over its quota)")
		case strings.Contains(m, "unable to authenticate") || strings.Contains(m, "access denied") || strings.Contains(m, "accessdenied") ||
			strings.Contains(m, "invalidaccesskeyid") || strings.Contains(m, "signaturedoesnotmatch") || strings.Contains(m, "logon failure") ||
			strings.Contains(m, "authenticationfailed") || strings.Contains(m, "401") || strings.Contains(m, "403"):
			hint = " " + l.T("(check the user name, password or key)")
		case strings.Contains(m, "host key") || strings.Contains(m, "fingerprint"):
			hint = " " + l.T("(the server presented another key or certificate than the one entered)")
		case strings.Contains(m, "connection refused") || strings.Contains(m, "no such host") || strings.Contains(m, "timeout") || strings.Contains(m, "deadline exceeded"):
			hint = " " + l.T("(check the address and the port, and that the console can reach the storage)")
		}
		return "", fmt.Errorf("%s: %s%s", name, l.T("connection test failed: %v", err), hint)
	}
	return name + ": " + l.T("connection test passed — %s", l.T(ok)), nil
}

func (s *Server) handleTargetTest(w http.ResponseWriter, r *http.Request, _ string) {
	id, _ := pathID(r)
	t, err := s.store.GetTarget(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	l := requestLanguage(r)
	ok, note, err := testTargetLock(r.Context(), t)
	msg, err := targetTestMessage(l, t.Name, ok, err)
	if err != nil {
		redirectErr(w, r, "/targets", err)
		return
	}
	redirectMsg(w, r, "/targets", msg+note.text(l))
}
