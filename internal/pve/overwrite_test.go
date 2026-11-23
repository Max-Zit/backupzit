package pve

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/vmlock"
)

// C1 (M-14, 0.59): a restore over an existing guest writes all disks first
// and deletes the old guest only then; a failure before that leaves it
// unchanged (started again if it was stopped). "Remove first" keeps the way
// before 0.59 for storage without room for both.
func TestOverwriteRestoreOrder(t *testing.T) {
	type scenario struct {
		name        string
		running     bool
		removeFirst bool
		writeErr    error
		destroyErr  bool
		configThere bool
		freeKiB     string // free space pvesm reports, default plenty
		wantCalls   []string
		wantErr     []string // substrings; none = success
		wantNoCall  []string
	}
	const (
		alloc   = "pvesm alloc local 100  1024 --format raw"
		free    = "pvesm free local:100/vm-100-disk-1.raw"
		stop    = "qm stop 100"
		destroy = "qm destroy 100 --destroy-unreferenced-disks 0"
		start   = "qm start 100"
	)
	for _, sc := range []scenario{
		{name: "safe order", running: true, wantCalls: []string{alloc, "write", stop, destroy}, wantNoCall: []string{free, start}},
		{name: "stopped guest is not stopped", wantCalls: []string{alloc, "write", destroy}, wantNoCall: []string{stop, free}},
		{name: "no room: old guest unchanged", running: true, writeErr: errors.New("write: no space left on device"),
			wantCalls: []string{alloc, "write", free}, wantNoCall: []string{stop, "qm destroy"},
			wantErr: []string{"existing guest 100 was not changed", "remove the existing guest first"}},
		{name: "repository error: old guest unchanged", running: true, writeErr: errors.New("data blob c377860a not found in index"),
			wantCalls: []string{alloc, "write", free}, wantNoCall: []string{stop, "qm destroy"},
			wantErr: []string{"existing guest 100 was not changed"}},
		{name: "delete fails: started again, new disks removed", running: true, destroyErr: true,
			wantCalls: []string{alloc, "write", stop, destroy, start, free}, wantErr: []string{"existing guest 100 was not changed"}},
		{name: "configuration fails after the delete: disks kept", running: true, configThere: true,
			wantCalls: []string{alloc, "write", stop, destroy}, wantNoCall: []string{free},
			wantErr: []string{"restored disks of ID 100 are kept: local:100/vm-100-disk-1.raw"}},
		{name: "remove first (as before 0.59)", running: true, removeFirst: true,
			wantCalls: []string{stop, "qm destroy 100", alloc, "write"}, wantNoCall: []string{destroy, free}},
		{name: "no room next to the old guest: nothing done", running: true, freeKiB: "1048576",
			wantNoCall: []string{"pvesm alloc", stop, "qm destroy"}, wantErr: []string{"storage local has 1.0 GiB free", "the guest was not changed", "delete the existing guest first"}},
		{name: "no room but delete first", running: true, removeFirst: true, freeKiB: "1048576",
			wantCalls: []string{stop, "qm destroy 100", alloc, "write"}},
	} {
		t.Run(sc.name, func(t *testing.T) {
			oldDir, oldCmd, oldLocks, oldWrite := ConfigDir, command, vmlock.Dir, writeDiskFn
			defer func() { ConfigDir, command, vmlock.Dir, writeDiskFn = oldDir, oldCmd, oldLocks, oldWrite }()
			ConfigDir, vmlock.Dir = t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(ConfigDir, "storage.cfg"), []byte("dir: local\n\tpath /var/lib/vz\n\tcontent images,rootdir\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cfgDir := filepath.Join(ConfigDir, "nodes", LocalNode(), "qemu-server")
			if err := os.MkdirAll(cfgDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if sc.configThere {
				if err := os.WriteFile(filepath.Join(cfgDir, "100.conf"), []byte("name: other\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			status := "stopped"
			if sc.running {
				status = "running"
			}
			var calls []string
			command = func(_ context.Context, name string, args ...string) ([]byte, error) {
				c := name + " " + strings.Join(args, " ")
				switch {
				case strings.HasPrefix(c, "pvesh get /cluster/resources"):
					return []byte(fmt.Sprintf(`[{"vmid":100,"name":"web","type":"qemu","node":%q,"status":%q}]`, LocalNode(), status)), nil
				case c == "pveversion":
					return []byte("pve-manager/9.2.21"), nil
				case c == "pvesm status --storage local":
					free := sc.freeKiB
					if free == "" {
						free = "34423880"
					}
					return []byte("Name         Type     Status     Total (KiB)      Used (KiB) Available (KiB)        %" + "\n" +
						"local         dir     active        98497780        59024352        " + free + "   59.92%" + "\n"), nil
				}
				calls = append(calls, c)
				switch {
				case strings.HasPrefix(c, "pvesm alloc"):
					return []byte("successfully created 'local:100/vm-100-disk-1.raw'"), nil
				case strings.HasPrefix(c, "qm destroy") && sc.destroyErr:
					return nil, errors.New("VM is locked (backup)")
				}
				return nil, nil
			}
			writeDiskFn = func(context.Context, *repo.Repository, *repo.Snapshot, repo.GuestDisk, storage, string, func(uint64)) error {
				calls = append(calls, "write")
				return sc.writeErr
			}
			sn := &repo.Snapshot{
				Guests: []repo.Guest{{Platform: "proxmox", VMID: 100, Type: "qemu", Name: "web", Config: "name: web\nscsi0: local:100/vm-100-disk-0.raw,size=1M\n",
					Disks: []repo.GuestDisk{{Key: "scsi0", Volume: "local:100/vm-100-disk-0.raw", Storage: "local", Size: 1 << 20, Image: 0}}}},
				Images: []repo.DiskImage{{Size: 1 << 20}},
			}
			_, err := Restore(context.Background(), nil, sn, RestoreOptions{VMID: 100, Overwrite: true, RemoveFirst: sc.removeFirst})
			if len(sc.wantErr) == 0 && err != nil {
				t.Fatalf("restore: %v (calls %q)", err, calls)
			}
			for _, w := range sc.wantErr {
				if err == nil || !strings.Contains(err.Error(), w) {
					t.Errorf("error %v, want %q", err, w)
				}
			}
			// The wanted calls in this order (others may come between).
			i := 0
			for _, c := range calls {
				if i < len(sc.wantCalls) && strings.HasPrefix(c, sc.wantCalls[i]) {
					i++
				}
			}
			if i != len(sc.wantCalls) {
				t.Errorf("calls %q, want in order %q", calls, sc.wantCalls)
			}
			for _, c := range calls {
				for _, no := range sc.wantNoCall {
					if strings.HasPrefix(c, no) {
						t.Errorf("unexpected %q (calls %q)", c, calls)
					}
				}
			}
			if len(sc.wantErr) == 0 {
				b, err := os.ReadFile(filepath.Join(cfgDir, "100.conf"))
				if err != nil || !strings.Contains(string(b), "local:100/vm-100-disk-1.raw") {
					t.Errorf("configuration: %q %v", b, err)
				}
			}
		})
	}
}

// pvesm status as Proxmox VE 9.2 prints it (KiB).
func TestStorageSpace(t *testing.T) {
	oldCmd := command
	defer func() { command = oldCmd }()
	command = func(_ context.Context, name string, args ...string) ([]byte, error) {
		return []byte("Name             Type     Status     Total (KiB)      Used (KiB) Available (KiB)        %\n" +
			"local-lvm     lvmthin     active       354275328       227763608       126511719   64.29%\n"), nil
	}
	total, avail, err := storageSpace(context.Background(), "local-lvm")
	if err != nil || total != 354275328*1024 || avail != 126511719*1024 {
		t.Fatalf("total %d avail %d %v", total, avail, err)
	}
	if _, _, err := storageSpace(context.Background(), "other"); err == nil {
		t.Fatal("a storage not in the output")
	}
}
