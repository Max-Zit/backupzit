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

// testTarget checks that the console can reach a storage target with its
// credentials: it writes, reads back and removes a small file, or only
// lists on storage where files cannot be removed (S3 Object Lock,
// hardened repository). Agents write the backups; the console tests from
// its own network.
func testTarget(ctx context.Context, t Target) (string, error) {
	if t.Kind == "local" || t.Kind == "usb" {
		return "", errors.New("a local or USB disk is only reachable by its agent; the first backup tests it")
	}
	ctx, cancel := context.WithTimeout(ctx, targetTestTimeout)
	defer cancel()
	opts := backend.Options{
		SFTPPassword: t.SFTPPassword, SFTPHostKey: t.SFTPHostKey,
		S3AccessKey: t.S3AccessKey, S3SecretKey: t.S3SecretKey, S3Region: t.S3Region, S3LockDays: t.S3LockDays,
		SMBPassword: t.SMBPassword, SMBDomain: t.SMBDomain,
		HardenedKey: t.HardenedKey, HardenedFingerprint: t.HardenedFingerprint,
		AzureKey: t.AzureKey, AzureSAS: t.AzureSAS,
	}
	if t.SFTPKey != "" {
		f, err := os.CreateTemp("", "bz-test-key-*")
		if err != nil {
			return "", err
		}
		defer os.Remove(f.Name())
		f.WriteString(t.SFTPKey)
		f.Close()
		opts.SFTPKeyFile = f.Name()
	}
	be, err := backend.Open(ctx, t.URL, opts)
	if err != nil {
		return "", fmt.Errorf("connect: %w", err)
	}
	defer be.Close()
	if t.S3LockDays > 0 || t.Kind == "hardened" {
		if _, err := be.List(ctx, "keys"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("list: %w", err)
		}
		return "connected and signed in (immutable storage: nothing written)", nil
	}
	b := make([]byte, 8)
	rand.Read(b)
	name := "backupzit-test-" + hex.EncodeToString(b)
	data := []byte("BackupZit connection test " + time.Now().UTC().Format(time.RFC3339))
	if err := be.Save(ctx, name, data); err != nil {
		return "", fmt.Errorf("write a test file: %w", err)
	}
	got, err := be.Load(ctx, name)
	if err == nil && !bytes.Equal(got, data) {
		err = errors.New("the file read back differs")
	}
	if rerr := be.Remove(ctx, name); err == nil && rerr != nil {
		err = fmt.Errorf("remove the test file: %w", rerr)
	}
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	return "connected, wrote, read and removed a test file", nil
}

// targetTestMessage words a test result for the page.
func targetTestMessage(l *language, name, ok string, err error) (string, error) {
	if err != nil {
		hint := ""
		switch m := strings.ToLower(err.Error()); {
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
	ok, err := testTarget(r.Context(), t)
	msg, err := targetTestMessage(requestLanguage(r), t.Name, ok, err)
	if err != nil {
		redirectErr(w, r, "/targets", err)
		return
	}
	redirectMsg(w, r, "/targets", msg)
}
