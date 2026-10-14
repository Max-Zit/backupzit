package vmware

import (
	"context"
	"crypto/tls"
	"strings"
	"testing"

	"github.com/vmware/govmomi/simulator"
)

// TestVCenterSimulator connects to a simulated vCenter: VMs of all hosts
// with their host, the host list for restores and placement on a host.
func TestVCenterSimulator(t *testing.T) {
	ctx := context.Background()
	model := simulator.VPX()
	model.Host = 1 // a standalone host besides the cluster
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
	if !c.VCenter || c.Method() != MethodAPI || !c.APIWrites {
		t.Fatalf("vCenter not recognised: vcenter=%v method=%s", c.VCenter, c.Method())
	}
	if _, err := Connect(ctx, Conn{Host: s.URL.Host, User: s.URL.User.Username(), Password: pw, Method: MethodSSH}); err == nil {
		t.Error("SSH accepted for vCenter")
	}
	hosts, err := c.Hosts(ctx)
	if err != nil || len(hosts) < 2 {
		t.Fatalf("hosts: %v %v", hosts, err)
	}
	vms, err := c.VMs(ctx)
	if err != nil || len(vms) == 0 {
		t.Fatalf("VMs: %v %v", vms, err)
	}
	for _, v := range vms {
		if v.Host == "" {
			t.Errorf("VM %s without its host", v.Name)
		}
		// The simulator's VMs have an instance and a BIOS UUID.
		if v.BIOSVMID == 0 || v.BIOSVMID == v.VMID {
			t.Errorf("VM %s: former number %d (number %d)", v.Name, v.BIOSVMID, v.VMID)
		}
	}
	inv, err := c.Inventory(ctx, "vc1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(inv.Version, "vCenter") || len(inv.Hosts) < 2 || inv.Guests[0].Node == "vc1" {
		t.Errorf("inventory: version %q hosts %v node %q", inv.Version, inv.Hosts, inv.Guests[0].Node)
	}
	// Placement on a named host and on the first one.
	want := hosts[len(hosts)-1].Name
	h, pool, err := c.hostAndPool(ctx, want)
	if err != nil || pool == nil {
		t.Fatal(err)
	}
	if name, _ := h.ObjectName(ctx); name != want {
		t.Errorf("placed on %s, want %s", name, want)
	}
	if _, _, err := c.hostAndPool(ctx, "no-such-host"); err == nil {
		t.Error("unknown host accepted")
	}
}
