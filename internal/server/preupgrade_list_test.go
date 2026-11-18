package server_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/server"
)

// The container image has no shell, so the copies made before updates are
// listed by the program: newest first, also in the error for a wrong name.
func TestPreUpgradeCopiesListed(t *testing.T) {
	data := t.TempDir()
	if got := server.PreUpgradeCopies(data); len(got) != 0 {
		t.Fatalf("copies without any: %v", got)
	}
	for _, n := range []string{"20261101-020000-from-0.50.0", "20261118-101500-from-0.51.0"} {
		if err := os.MkdirAll(filepath.Join(data, server.PreUpgradeDir, n), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"20261118-101500-from-0.51.0", "20261101-020000-from-0.50.0"}
	if got := server.PreUpgradeCopies(data); !reflect.DeepEqual(got, want) {
		t.Errorf("copies %v, want %v", got, want)
	}
	err := server.RestorePreUpgrade(context.Background(), "", data, filepath.Join(data, server.PreUpgradeDir, "nonsense"), t.Logf)
	if err == nil || !strings.Contains(err.Error(), "the copies are: "+want[0]) {
		t.Errorf("wrong name: %v", err)
	}
}
