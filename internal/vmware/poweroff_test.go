package vmware

import (
	"context"
	"crypto/tls"
	"testing"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/simulator"
)

// M-16 (0.55): powerOff also turns off a suspended VM (an in-place restore
// must not let it resume old memory on top of restored disks).
func TestPowerOffSuspended(t *testing.T) {
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
	vms, err := c.VMs(ctx)
	if err != nil || len(vms) == 0 {
		t.Fatalf("VMs: %v %v", vms, err)
	}
	vm := vms[0]
	ov := object.NewVirtualMachine(c.vim, vm.Ref)
	if st := c.powerState(ctx, vm.MoID); st != "poweredOn" {
		tk, _ := ov.PowerOn(ctx)
		_ = tk.Wait(ctx)
	}
	tk, err := ov.Suspend(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tk.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if st := c.powerState(ctx, vm.MoID); st != "suspended" {
		t.Fatalf("state after suspend: %q", st)
	}
	if err := c.powerOff(ctx, vm.MoID); err != nil {
		t.Fatal(err)
	}
	if st := c.powerState(ctx, vm.MoID); st != "poweredOff" {
		t.Fatalf("suspended VM not powered off: %q", st)
	}
	if err := c.powerOff(ctx, vm.MoID); err != nil {
		t.Errorf("powering off a VM that is off: %v", err)
	}
}
