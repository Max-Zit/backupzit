package vmware

import (
	"context"
	"crypto/tls"
	"errors"
	"testing"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/simulator"
)

// #44: when the vSphere session ends during a run (expired, or the host's
// management services restarted), the proxy signs in again: the snapshot
// of the VM just read is still removed and the next calls work, instead of
// "The session is not authenticated" and 401 for every later VM.
func TestSignInAgainAfterTheSessionEnded(t *testing.T) {
	ctx := context.Background()
	model := simulator.ESX()
	defer model.Remove()
	if err := model.Create(); err != nil {
		t.Fatal(err)
	}
	model.Service.TLS = new(tls.Config)
	s := model.Service.NewServer()
	defer s.Close()
	pw, _ := s.URL.User.Password()
	c, err := Connect(ctx, Conn{Host: s.URL.Host, User: s.URL.User.Username(), Password: pw})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Logout()
	admin, err := govmomi.NewClient(ctx, s.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Logout(ctx) }()
	end := func() {
		t.Helper()
		us, err := session.NewManager(c.vim).UserSession(ctx)
		if err != nil || us == nil {
			t.Fatalf("session of the proxy: %v %v", us, err)
		}
		if err := session.NewManager(admin.Client).TerminateSession(ctx, []string{us.Key}); err != nil {
			t.Fatal(err)
		}
	}

	// A live session is left alone.
	if err := c.ensureSession(ctx); err != nil || c.Relogins != 0 {
		t.Fatalf("live session: %v, %d sign-ins", err, c.Relogins)
	}
	vms, err := c.VMs(ctx)
	if err != nil || len(vms) == 0 {
		t.Fatalf("VMs: %v %v", vms, err)
	}
	vm := vms[0]

	end()
	if _, err := c.hasSnapshots(ctx, vm.Ref); !sessionLost(err) {
		t.Fatalf("call after the session ended: %v (want NotAuthenticated)", err)
	}
	if err := c.again(ctx, func() error { _, err := c.hasSnapshots(ctx, vm.Ref); return err }); err != nil || c.Relogins != 1 {
		t.Fatalf("again: %v, %d sign-ins", err, c.Relogins)
	}

	// The backup snapshot is removed although the session ended while the
	// disks were read.
	tk, err := object.NewVirtualMachine(admin.Client, vm.Ref).CreateSnapshot(ctx, "backupzit_1700000001", "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := tk.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	end()
	if _, err := c.removeSnapshotVerified(ctx, vm, "backupzit_1700000001", nil); err != nil {
		t.Fatalf("remove after the session ended: %v", err)
	}
	if own, err := c.hasSnapshots(ctx, vm.Ref); err != nil || own {
		t.Fatalf("snapshot left: %v %v", own, err)
	}
	if c.Relogins != 2 {
		t.Errorf("%d sign-ins, want 2", c.Relogins)
	}

	if !sessionLost(errors.New("download [ds] vm/vm.vmx: 401 Unauthorized")) || sessionLost(errors.New("404 Not Found")) {
		t.Error("sessionLost misreads HTTP answers")
	}
}
