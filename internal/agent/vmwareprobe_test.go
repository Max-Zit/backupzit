package agent

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/vmware/govmomi/simulator"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/pve"
)

// V-3 (#51): the proxy checks a VMware host for the console, reports the
// certificate it saw on first contact and refuses a changed one later.
func TestVMwareProbe(t *testing.T) {
	model := simulator.ESX()
	defer model.Remove()
	if err := model.Create(); err != nil {
		t.Fatal(err)
	}
	model.Service.TLS = new(tls.Config)
	sim := model.Service.NewServer()
	defer sim.Close()
	pass, _ := sim.URL.User.Password()
	a := New(&Config{ServerURL: "https://console.invalid", AgentUUID: "u", Secret: "s"}, slog.New(slog.NewTextHandler(io.Discard, nil)), "0.56.0")
	host := &api.VMwareHost{Name: "esx1", Address: sim.URL.Host, User: sim.URL.User.Username(), Password: pass}
	res := a.vmwareProbe(context.Background(), api.Run{Kind: api.KindVMwareProbe, VMware: host})
	if res.Status != api.StatusSuccess {
		t.Fatalf("probe: %s", res.Message)
	}
	var p api.VMwareProbe
	if err := json.Unmarshal(res.Details, &p); err != nil || !strings.HasPrefix(p.Thumbprint, "SHA256:") || p.Version == "" {
		t.Fatalf("details %s: %v", res.Details, err)
	}
	var inv pve.Inventory
	if err := json.Unmarshal(p.Inventory, &inv); err != nil || len(inv.Guests) == 0 || inv.Platform != "vmware" {
		t.Fatalf("inventory %s: %v", p.Inventory, err)
	}
	// Pinned: the same certificate passes, another one is refused with the
	// fingerprint the console offers to accept.
	host.Thumbprint = p.Thumbprint
	if res := a.vmwareProbe(context.Background(), api.Run{Kind: api.KindVMwareProbe, VMware: host}); res.Status != api.StatusSuccess {
		t.Fatalf("pinned probe: %s", res.Message)
	}
	host.Thumbprint = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	res = a.vmwareProbe(context.Background(), api.Run{Kind: api.KindVMwareProbe, VMware: host})
	if res.Status != api.StatusFailed || !strings.Contains(res.Message, "certificate changed ("+p.Thumbprint+", expected") {
		t.Fatalf("changed certificate: %s %s", res.Status, res.Message)
	}
	if res := a.vmwareProbe(context.Background(), api.Run{Kind: api.KindVMwareProbe}); res.Status != api.StatusFailed {
		t.Fatal("a probe without a host succeeded")
	}
}
