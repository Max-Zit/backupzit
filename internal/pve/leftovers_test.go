package pve

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/leftover"
	"github.com/max-zit/backupzit/internal/vmlock"
)

// S-2 (0.56): the janitor removes the snapshots and backup locks in the
// ledger and old BackupZit snapshots it does not know, and leaves guests
// alone that a run (VM lock) or a Proxmox VE task uses.
func TestCleanLeftovers(t *testing.T) {
	oldDir, oldCmd, oldLocks := ConfigDir, command, vmlock.Dir
	defer func() { ConfigDir, command, vmlock.Dir = oldDir, oldCmd, oldLocks }()
	ConfigDir, vmlock.Dir = t.TempDir(), t.TempDir()
	leftover.Open(filepath.Join(t.TempDir(), "leftovers.json"))
	defer leftover.Close()
	dir := filepath.Join(ConfigDir, "nodes", LocalNode(), "qemu-server")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := strconv.FormatInt(time.Now().Add(-48*time.Hour).Unix(), 10)
	young := strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10)
	confs := map[int]string{
		// A crash during the creation: Proxmox VE's snapshot lock stays.
		101: "lock: snapshot\nscsi0: local-lvm:vm-101-disk-0,size=1G\n\n[backupzit_" + young + "]\nsnapstate: prepare\n",
		// A pre-0.56 leftover nobody recorded, and a young one (a backup on
		// another node may still read it).
		102: "scsi0: local-lvm:vm-102-disk-0,size=1G\n\n[backupzit_" + old + "]\nscsi0: x\n\n[before-upgrade]\nscsi0: y\n",
		103: "scsi0: local-lvm:vm-103-disk-0,size=1G\n\n[backupzit_" + young + "]\nscsi0: x\n",
		// The backup lock a crashed run set.
		104: "lock: backup\nscsi0: local-lvm:vm-104-disk-0,size=1G\n",
		// A vzdump task runs for this one.
		105: "lock: backup\nscsi0: local-lvm:vm-105-disk-0,size=1G\n",
		// A run of this agent works on this one.
		106: "scsi0: local-lvm:vm-106-disk-0,size=1G\n\n[backupzit_" + old + "]\nscsi0: x\n",
	}
	var mu sync.Mutex
	path := func(id int) string { return filepath.Join(dir, strconv.Itoa(id)+".conf") }
	for id, c := range confs {
		if err := os.WriteFile(path(id), []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var calls []string
	command = func(_ context.Context, name string, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		cmd := name + " " + strings.Join(args, " ")
		calls = append(calls, cmd)
		switch {
		case strings.Contains(cmd, "/tasks"):
			if strings.Contains(cmd, "--vmid 105") {
				return []byte(`[{"upid":"UPID:pve:vzdump"}]`), nil
			}
			return []byte("[]"), nil
		case name == "qm" && args[0] == "unlock":
			id, _ := strconv.Atoi(args[1])
			b, _ := os.ReadFile(path(id))
			var keep []string
			for _, l := range strings.Split(string(b), "\n") {
				if !strings.HasPrefix(l, "lock:") {
					keep = append(keep, l)
				}
			}
			return nil, os.WriteFile(path(id), []byte(strings.Join(keep, "\n")), 0o600)
		case name == "qm" && args[0] == "delsnapshot":
			id, _ := strconv.Atoi(args[1])
			b, _ := os.ReadFile(path(id))
			if strings.Contains(string(b), "lock: snapshot") {
				return nil, errors.New("VM is locked (snapshot)")
			}
			c := string(b)
			i := strings.Index(c, "\n["+args[2]+"]")
			if i < 0 {
				return nil, errors.New("snapshot does not exist")
			}
			rest := c[i+1:]
			if j := strings.Index(rest[1:], "\n["); j >= 0 {
				c = c[:i] + rest[j+1:]
			} else {
				c = c[:i] + "\n"
			}
			return nil, os.WriteFile(path(id), []byte(c), 0o600)
		}
		return nil, errors.New("not in this test: " + cmd)
	}
	ctx := context.Background()
	leftover.Add(ctx, snapshotItem(Guest{VMID: 101, Type: "qemu"}, "backupzit_"+young))
	leftover.Add(ctx, lockItem(Guest{VMID: 104, Type: "qemu"}))
	leftover.Add(ctx, lockItem(Guest{VMID: 105, Type: "qemu"}))
	unlock, err := vmlock.Lock(ctx, vmLockKey(106))
	if err != nil {
		t.Fatal(err)
	}
	removed, err := CleanLeftovers(ctx, nil)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	read := func(id int) string { b, _ := os.ReadFile(path(id)); return string(b) }
	if c := read(101); strings.Contains(c, "backupzit_") || strings.Contains(c, "lock:") {
		t.Errorf("101 still has the crashed snapshot or its lock:\n%s", c)
	}
	if c := read(102); strings.Contains(c, "backupzit_") || !strings.Contains(c, "[before-upgrade]") {
		t.Errorf("102: the old BackupZit snapshot stays or the user's went:\n%s", c)
	}
	if c := read(103); !strings.Contains(c, "backupzit_"+young) {
		t.Error("103: a young unknown snapshot was removed")
	}
	if c := read(104); strings.Contains(c, "lock:") {
		t.Error("104: the backup lock BackupZit set stays")
	}
	if c := read(105); !strings.Contains(c, "lock: backup") {
		t.Error("105: the lock was removed while a Proxmox VE task runs")
	}
	if c := read(106); !strings.Contains(c, "backupzit_") {
		t.Error("106: a guest a run works on was touched")
	}
	if len(removed) != 3 {
		t.Errorf("removed %q, want 101, 102 and 104", removed)
	}
	// What is left in the ledger: 105 (busy). The snapshot of 106 is not a
	// leftover while a run works on the guest.
	var left []string
	for _, it := range leftover.List() {
		left = append(left, it.VM)
	}
	if strings.Join(left, ",") != "105" {
		t.Errorf("ledger %v, want 105", left)
	}
}
