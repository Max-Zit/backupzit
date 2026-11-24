package server

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/recovery"
)

func TestCredentialsFingerprint(t *testing.T) {
	a := Target{ID: 1, Name: "NAS", URL: "smb://bz@nas/backups", SMBPassword: "one", RecoveryKey: "key"}
	b := a
	if credentialsFingerprint(a) != credentialsFingerprint(b) {
		t.Fatal("fingerprint not stable")
	}
	b.SMBPassword = "two"
	if credentialsFingerprint(a) == credentialsFingerprint(b) {
		t.Error("changed password not noticed")
	}
	b = a
	b.Name = "renamed"
	if credentialsFingerprint(a) != credentialsFingerprint(b) {
		t.Error("renaming changes the fingerprint")
	}
}

func TestStaleAccessPackages(t *testing.T) {
	nas := Target{ID: 1, Name: "NAS", URL: "smb://bz@nas/backups", SMBPassword: "one"}
	s3 := Target{ID: 2, Name: "S3", URL: "s3://s3.example/b", S3SecretKey: "x"}
	rec := accessRecord{Created: time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC), User: "admin",
		Targets: map[int64]string{1: credentialsFingerprint(nas), 2: credentialsFingerprint(s3)}}
	if got := staleAccessPackages([]accessRecord{rec}, []Target{nas, s3}); len(got) != 0 {
		t.Errorf("fresh package reported stale: %v", got)
	}
	nas.SMBPassword = "two"
	got := staleAccessPackages([]accessRecord{rec}, []Target{nas})
	if len(got) != 1 || !strings.Contains(got[0], "NAS") || !strings.Contains(got[0], "removed") {
		t.Errorf("stale: %v", got)
	}
}

// Every reason internal/recovery gives for refusing a passphrase or a
// package has its own console text, and every language translates it.
func TestAccessMessages(t *testing.T) {
	for _, err := range []error{recovery.ErrPassphraseShort, recovery.ErrPassphraseControl, recovery.ErrPassphraseSpace,
		recovery.ErrPassphraseCommon, recovery.ErrPassphraseKinds, recovery.ErrPassphrasePredictable, recovery.ErrAccessPackageTooLarge} {
		wrapped := fmt.Errorf("x: %w", err)
		if msg := accessMessage(wrapped); msg == err.Error() || msg == wrapped.Error() {
			t.Errorf("%v has no console text", err)
		}
	}
	keys := []string{msgConsolePassword}
	for _, m := range accessMessages {
		keys = append(keys, m.msg)
	}
	for _, l := range languages {
		if l.Code == "en" {
			continue
		}
		for _, k := range keys {
			if l.msgs[k] == "" {
				t.Errorf("%s: %q not translated", l.Code, k)
			}
		}
	}
	if got := accessMessage(errors.New("something else")); got != "something else" {
		t.Errorf("unknown error: %q", got)
	}
}

func TestAccessRepoFromTarget(t *testing.T) {
	tg := Target{ID: 1, Name: "NAS", URL: "smb://bz@nas/backups", SMBPassword: "pw", SMBDomain: "CORP", Encrypted: true, RecoveryKey: "rk"}
	a := accessRepo(tg, "pc1 – NAS", "smb://bz@nas/backups/pc1")
	if a.Location != "smb://bz@nas/backups/pc1" || a.Password != "rk" || a.SMBPassword != "pw" || a.SMBDomain != "CORP" {
		t.Errorf("access repo %+v", a)
	}
	b, err := recovery.SealAccessPackage(&recovery.AccessPackage{Repositories: []recovery.AccessRepo{a}}, "a long enough passphrase")
	if err != nil {
		t.Fatal(err)
	}
	p, err := recovery.OpenAccessPackage(b, "a long enough passphrase")
	if err != nil || p.Repositories[0].SMBPassword != "pw" {
		t.Errorf("round trip: %+v, %v", p, err)
	}
}
