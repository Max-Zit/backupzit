package pve

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// H-6 (0.55): a replica that was started (through BackupZit or in the
// Proxmox VE interface) is compared block by block at the next update.
func TestReplicaDiverged(t *testing.T) {
	oldDir, oldCmd := ConfigDir, command
	defer func() { ConfigDir, command = oldDir, oldCmd }()
	ConfigDir = t.TempDir()
	var startTasks string
	command = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "pvesh" && strings.Contains(strings.Join(args, " "), "--typefilter qmstart") {
			return []byte(startTasks), nil
		}
		if name == "qm" {
			return nil, nil // start
		}
		return nil, errors.New("not here")
	}
	at := time.Unix(1_800_000_000, 0)
	conf := "description: BackupZit replica of guest 100 (web), updated from the backup of 2027-01-15 08:00. " + replicaState("abc123", at) + "\nmemory: 2048\n"

	startTasks = "[]"
	if why := replicaDiverged(context.Background(), conf, "abc123", "qemu", 9100); why != "" {
		t.Errorf("an untouched replica counts as diverged: %s", why)
	}
	if why := replicaDiverged(context.Background(), conf, "def456", "qemu", 9100); why == "" {
		t.Error("a replica holding another backup does not count as diverged")
	}
	if why := replicaDiverged(context.Background(), "description: BackupZit replica of guest 100 (web)\n", "abc123", "qemu", 9100); why == "" {
		t.Error("a replica without the state (older version) does not count as diverged")
	}
	startTasks = `[{"starttime":1800000600,"type":"qmstart","id":"9100"}]`
	if why := replicaDiverged(context.Background(), conf, "abc123", "qemu", 9100); !strings.Contains(why, "started") {
		t.Errorf("a start in the Proxmox VE interface is not noticed: %q", why)
	}

	// StartReplica removes the state before it starts the replica.
	startTasks = "[]"
	dir := filepath.Join(ConfigDir, "qemu-server")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "9100.conf")
	if err := os.WriteFile(p, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := StartReplica(context.Background(), 9100, 100); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), "BackupZit state") || !strings.Contains(string(b), replicaMarker(100)) {
		t.Fatalf("config after the start:\n%s", b)
	}
	if why := replicaDiverged(context.Background(), string(b), "abc123", "qemu", 9100); why == "" {
		t.Error("a replica started through BackupZit does not count as diverged")
	}
}
