package mssql

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/microsoft/go-mssqldb/msdsn"
)

// #78: no deadline on reads and writes after the login. "connection
// timeout" is one in go-mssqldb, so a BACKUP DATABASE that ran longer than
// it on the server failed with "Pipe IO timed out".
func TestDSNHasNoReadDeadline(t *testing.T) {
	for _, host := range []string{"lpc:.", `np:\\.\pipe\sql\query`, "tcp:localhost"} {
		cfg, err := msdsn.Parse(dsn(host))
		if err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		if cfg.ConnTimeout != 0 {
			t.Errorf("%s: connection timeout %v, want none", host, cfg.ConnTimeout)
		}
		if cfg.DialTimeout != 10*time.Second {
			t.Errorf("%s: dial timeout %v, want 10 s", host, cfg.DialTimeout)
		}
		if cfg.Encryption != msdsn.EncryptionDisabled {
			t.Errorf("%s: encryption %v, want disabled", host, cfg.Encryption)
		}
	}
}

// The permission hint names the SYSTEM account as SQL Server knows it.
func TestPermissionHintNamesSystem(t *testing.T) {
	err := permissionHint(errors.New("mssql: Login failed for user"))
	if err == nil || !strings.Contains(err.Error(), "ADD MEMBER ["+systemAccount()+"]") || systemAccount() == "" {
		t.Fatalf("hint %v (account %q)", err, systemAccount())
	}
}
