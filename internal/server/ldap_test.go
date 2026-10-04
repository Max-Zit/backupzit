package server

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/max-zit/backupzit/internal/tlsutil"
)

// TestLDAPLive runs against a real directory, e.g. the lab OpenLDAP:
//
//	BACKUPZIT_TEST_LDAP=ldaps://host:636 BACKUPZIT_TEST_LDAP_BIND_PW=... BACKUPZIT_TEST_LDAP_USER_PW=... go test -run LDAPLive
//
// It expects dc=lab,dc=local with ana (admins), marko (operators) and petar
// (no group), all with the user password.
func TestLDAPLive(t *testing.T) {
	url := os.Getenv("BACKUPZIT_TEST_LDAP")
	if url == "" {
		t.Skip("BACKUPZIT_TEST_LDAP not set")
	}
	ctx := context.Background()
	fp, err := tlsutil.FetchFingerprint(ctx, url[len("ldaps://"):])
	if err != nil {
		t.Fatal(err)
	}
	pw := os.Getenv("BACKUPZIT_TEST_LDAP_USER_PW")
	l := LDAPSettings{Enabled: true, URL: url, Fingerprint: fp,
		BindDN: "cn=svc-backupzit,ou=services,dc=lab,dc=local", BindPassword: os.Getenv("BACKUPZIT_TEST_LDAP_BIND_PW"),
		BaseDN: "dc=lab,dc=local", UserFilter: ldapUserFilter,
		AdminGroup:    "cn=BackupZit Admins,ou=groups,dc=lab,dc=local",
		OperatorGroup: "cn=BackupZit Operators,ou=groups,dc=lab,dc=local",
	}
	if err := l.Validate(); err != nil {
		t.Fatal(err)
	}
	res, err := l.Authenticate(ctx, "ana", pw)
	if err != nil || res.Role != "admin" || res.DisplayName != "Ana Petrović" || res.Email != "ana@lab.local" {
		t.Fatalf("ana: %+v %v", res, err)
	}
	if res, err := l.Authenticate(ctx, "marko", pw); err != nil || res.Role != "operator" {
		t.Fatalf("marko: %+v %v", res, err)
	}
	if _, err := l.Authenticate(ctx, "petar", pw); !errors.Is(err, errNoRole) {
		t.Errorf("petar without group: %v", err)
	}
	if _, err := l.Authenticate(ctx, "ana", pw+"x"); !errors.Is(err, errBadLogin) {
		t.Errorf("wrong password: %v", err)
	}
	if _, err := l.Authenticate(ctx, "ana", ""); !errors.Is(err, errBadLogin) {
		t.Errorf("empty password: %v", err)
	}
	if _, err := l.Authenticate(ctx, "*", pw); !errors.Is(err, errBadLogin) {
		t.Errorf("wildcard user: %v", err)
	}
	bad := l
	bad.Fingerprint = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if _, err := bad.Authenticate(ctx, "ana", pw); err == nil {
		t.Error("wrong certificate accepted")
	}
	noPin := l
	noPin.Fingerprint = ""
	if _, err := noPin.Authenticate(ctx, "ana", pw); err == nil {
		t.Error("untrusted self-signed certificate accepted")
	}
	if msg, err := l.Test(ctx, "marko", ""); err != nil {
		t.Errorf("test lookup: %v", err)
	} else {
		t.Log(msg)
	}
}
