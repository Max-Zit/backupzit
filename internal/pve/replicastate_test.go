package pve

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/vmlock"
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

// L-N8 (0.56): a task log that cannot be read (pvesh fails, or prints
// something other than the task list) counts as a possible start, so the
// next update compares every block instead of trusting the replica.
func TestReplicaDivergedWhenTaskLogUnreadable(t *testing.T) {
	oldCmd := command
	defer func() { command = oldCmd }()
	conf := "description: " + replicaMarker(100) + " (web). " + replicaState("abc123", time.Unix(1_800_000_000, 0)) + "\n"
	for name, reply := range map[string]func() ([]byte, error){
		"pvesh fails":   func() ([]byte, error) { return nil, errors.New("ipcc_send_rec[1] failed: Connection refused") },
		"not JSON":      func() ([]byte, error) { return []byte("ipcc_send_rec failed"), nil },
		"no start time": func() ([]byte, error) { return []byte(`[{"type":"qmstart","id":"9100"}]`), nil },
	} {
		command = func(context.Context, string, ...string) ([]byte, error) { return reply() }
		if why := replicaDiverged(context.Background(), conf, "abc123", "qemu", 9100); !strings.Contains(why, "task log could not be read") {
			t.Errorf("%s: %q", name, why)
		}
	}
}

// L-N21 (0.56): an update removes the state from the replica's description
// before it writes the first disk and sets it again only when every disk is
// written. An update cut short (here growing the disk fails; in the field an
// agent or node crash) leaves the next update comparing every block.
func TestReplicaUpdateClearsStateBeforeWriting(t *testing.T) {
	oldDir, oldCmd, oldLocks := ConfigDir, command, vmlock.Dir
	defer func() { ConfigDir, command, vmlock.Dir = oldDir, oldCmd, oldLocks }()
	ConfigDir, vmlock.Dir = t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(ConfigDir, "storage.cfg"), []byte("lvmthin: local-lvm\n\tthinpool data\n\tvgname pve\n\tcontent rootdir,images\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	backup := func(id string, size uint64) *repo.Snapshot {
		return &repo.Snapshot{ID: repo.Hash([]byte(id)), Images: []repo.DiskImage{{Size: size}},
			Guests: []repo.Guest{{Platform: "proxmox", Type: "qemu", VMID: 100, Name: "web", Disks: []repo.GuestDisk{{Key: "scsi0", Size: size, Image: 0}}}}}
	}
	prev, sn := backup("monday", 1<<30), backup("tuesday", 2<<30)
	dir := filepath.Join(ConfigDir, "qemu-server")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "9100.conf")
	conf := "description: " + replicaMarker(100) + " (web), updated from the backup of 2027-01-15 08:00. " +
		replicaState(prev.ID.String(), time.Now().Add(-time.Hour)) + "\nscsi0: local-lvm:vm-9100-disk-0,size=1G\n"
	if err := os.WriteFile(p, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	wrote := false
	command = func(_ context.Context, name string, args ...string) ([]byte, error) {
		cmd := name + " " + strings.Join(args, " ")
		switch {
		case strings.Contains(cmd, "/cluster/resources"):
			return []byte(`[{"vmid":9100,"name":"web-replica","type":"qemu","node":"` + LocalNode() + `","status":"stopped"}]`), nil
		case strings.Contains(cmd, "/tasks"):
			return []byte("[]"), nil
		case name == "qm" && len(args) > 0 && args[0] == "resize":
			// The first write: the state must be gone by now.
			wrote = true
			if b, _ := os.ReadFile(p); strings.Contains(string(b), "BackupZit state") {
				t.Errorf("the disk is written while the replica still claims to hold the old backup:\n%s", b)
			}
			return nil, errors.New("resize: storage full")
		}
		return nil, errors.New("not in this test")
	}
	if _, err := Replicate(context.Background(), nil, sn, ReplicaOptions{VMID: 100, ReplicaVMID: 9100, Prev: prev}); err == nil || !strings.Contains(err.Error(), "storage full") {
		t.Fatalf("update: %v", err)
	}
	if !wrote {
		t.Fatal("the update did not reach the disk")
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), "BackupZit state") || !strings.Contains(string(b), replicaMarker(100)) {
		t.Fatalf("config after the failed update:\n%s", b)
	}
	if why := replicaDiverged(context.Background(), string(b), prev.ID.String(), "qemu", 9100); why == "" {
		t.Error("the next update would write only the changes over the partly written disks")
	}
}
