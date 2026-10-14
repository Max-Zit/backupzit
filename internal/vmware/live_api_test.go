package vmware

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/vmware/govmomi/object"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
)

// TestLiveAPIBackupRestore backs up a running VM of a licensed host through
// the API only, restores it as a new VM and checks the restored disks hold
// the same data:
//
//	BACKUPZIT_TEST_ESXI=192.168.101.100 BACKUPZIT_TEST_ESXI_PASSWORD=… BACKUPZIT_TEST_ESXI_VM=esx-deb go test ./internal/vmware -run LiveAPI -v
func TestLiveAPIBackupRestore(t *testing.T) {
	host, name := os.Getenv("BACKUPZIT_TEST_ESXI"), os.Getenv("BACKUPZIT_TEST_ESXI_VM")
	if host == "" || name == "" {
		t.Skip("BACKUPZIT_TEST_ESXI / BACKUPZIT_TEST_ESXI_VM not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	user := os.Getenv("BACKUPZIT_TEST_ESXI_USER")
	if user == "" {
		user = "root"
	}
	c, err := Connect(ctx, Conn{Host: host, User: user, Password: os.Getenv("BACKUPZIT_TEST_ESXI_PASSWORD"), Method: MethodAPI})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Logout()
	if c.Method() != MethodAPI {
		t.Fatal("not using the API")
	}
	// No SSH may be used on this path.
	c.conn.SSHPort = 1
	vms, err := c.VMs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var src *VM
	for i := range vms {
		if vms[i].Name == name {
			src = &vms[i]
		}
	}
	if src == nil {
		t.Fatalf("VM %s not found", name)
	}
	wasOn := src.Power == "poweredOn"
	if !wasOn {
		if err := c.powerOn(ctx, src.MoID); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Second)
	}
	defer func() {
		if !wasOn {
			c.powerOff(context.Background(), src.MoID)
		}
	}()
	be, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	sn, err := c.Backup(ctx, r, BackupOptions{VMIDs: []int{src.VMID}, Log: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("backup: %s, %s", sn.Guests[0].Consistency, sn.Images[0].Partitions[0].Method)
	newName := name + "-apitest"
	res, err := c.Restore(ctx, r, sn, RestoreOptions{VMID: src.VMID, AsNew: true, Name: newName, Log: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("restored %s (%s), notes %v", res.Name, res.MoID, res.Notes)
	defer func() {
		vm := object.NewVirtualMachine(c.vim, vmRef(res.MoID))
		if task, err := vm.Destroy(context.Background()); err == nil {
			task.Wait(context.Background())
		}
	}()
	// Back up the restored VM too: equal block IDs mean equal data.
	vms, _ = c.VMs(ctx)
	var dst *VM
	for i := range vms {
		if vms[i].MoID == res.MoID {
			dst = &vms[i]
		}
	}
	if dst == nil {
		t.Fatal("restored VM not in the inventory")
	}
	sn2, err := c.Backup(ctx, r, BackupOptions{VMIDs: []int{dst.VMID}, Log: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	for i, d := range sn.Guests[0].Disks {
		a, err := r.LoadBlockMap(ctx, &sn.Images[d.Image].Partitions[0])
		if err != nil {
			t.Fatal(err)
		}
		b, err := r.LoadBlockMap(ctx, &sn2.Images[sn2.Guests[0].Disks[i].Image].Partitions[0])
		if err != nil {
			t.Fatal(err)
		}
		if len(a) != len(b) {
			t.Fatalf("disk %s: %d blocks, restored %d", d.Key, len(a), len(b))
		}
		diff := 0
		for j := range a {
			if a[j] != b[j] {
				diff++
			}
		}
		if diff > 0 {
			t.Errorf("disk %s: %d of %d blocks differ after the restore", d.Key, diff, len(a))
		} else {
			t.Logf("disk %s: all %d blocks equal", d.Key, len(a))
		}
	}
}
