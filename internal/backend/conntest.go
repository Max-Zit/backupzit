package backend

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// ConnCheck is what a connection test found on a storage. The console runs
// it from its own network, an agent (target-test run) from the agent's.
type ConnCheck struct {
	// Immutable: the storage keeps what is written (S3 Object Lock, a
	// hardened repository), so the test only signed in and listed.
	Immutable bool
	// RetentionChecked: the default Object Lock retention of the S3 bucket
	// was read (RetentionMode, RetentionDays), or RetentionErr says why not.
	RetentionChecked bool
	RetentionMode    string
	RetentionDays    int
	RetentionErr     error
}

// Summary words a passed test for people (English; the console translates
// it).
func (c ConnCheck) Summary() string {
	if c.Immutable {
		return "connected and signed in (immutable storage: nothing written)"
	}
	return "connected, wrote, read and removed a test file"
}

// ImmutableLocation tells whether a storage location keeps what is written
// (an S3 bucket written with Object Lock for lockDays, or a hardened
// repository): a test must not leave a locked file behind there.
func ImmutableLocation(location string, lockDays int) bool {
	return lockDays > 0 || strings.HasPrefix(location, "hardened://")
}

// CheckConnection checks that be (opened at location with its credentials)
// can hold backups: it writes a small file, reads it back and removes it.
// On immutable storage (see ImmutableLocation) it only lists, so nothing
// locked stays behind, and for an S3 bucket with Object Lock it also reads
// the bucket's default retention. The error names the step that failed;
// connecting is the caller's step.
func CheckConnection(ctx context.Context, be Backend, location string, lockDays int) (ConnCheck, error) {
	var res ConnCheck
	if ImmutableLocation(location, lockDays) {
		res.Immutable = true
		if _, err := be.List(ctx, "keys"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return res, fmt.Errorf("list: %w", err)
		}
		if lockDays > 0 {
			if mode, days, ok, err := DefaultRetentionOf(ctx, be); ok {
				res.RetentionChecked, res.RetentionMode, res.RetentionDays, res.RetentionErr = true, mode, days, err
			}
		}
		return res, nil
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	name := "backupzit-test-" + hex.EncodeToString(b)
	data := []byte("BackupZit connection test " + time.Now().UTC().Format(time.RFC3339))
	if err := be.Save(ctx, name, data); err != nil {
		return res, fmt.Errorf("write a test file: %w", err)
	}
	// The file is removed also when reading failed, so no test file stays.
	got, err := be.Load(ctx, name)
	switch {
	case err != nil:
		err = fmt.Errorf("read: %w", err)
	case !bytes.Equal(got, data):
		err = errors.New("read: the file read back differs")
	}
	if rerr := be.Remove(ctx, name); err == nil && rerr != nil {
		err = fmt.Errorf("remove the test file: %w", rerr)
	}
	return res, err
}
