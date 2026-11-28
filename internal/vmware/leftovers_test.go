package vmware

import (
	"context"
	"crypto/tls"
	"path/filepath"
	"testing"
	"time"

	"github.com/vmware/govmomi/simulator"

	"github.com/max-zit/backupzit/internal/leftover"
)

// A leftover of a VM that is not in the VM list is kept for a grace period
// (the VM may be missing for a while: datastore down, moved and back), and
// dropped only after it (LOW: VMware ledger).
func TestCleanLeftoversKeepsMissingVM(t *testing.T) {
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
	leftover.Open(filepath.Join(t.TempDir(), "leftovers.json"))
	defer leftover.Close()
	it := leftover.Item{Platform: leftover.VMware, Kind: leftover.KindSnapshot, Host: c.conn.Host, VM: "50219999-0000-0000-0000-000000000000",
		VMName: "gone", Name: snapPrefix + "1763600000", Created: time.Now().Add(-time.Hour)}
	leftover.Add(ctx, it)
	if _, err := c.CleanLeftovers(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	got := leftover.List()
	if len(got) != 1 || got[0].MissingSince.IsZero() {
		t.Fatalf("the item of a missing VM: %+v", got)
	}
	old := leftover.MissingGrace
	leftover.MissingGrace = 0
	defer func() { leftover.MissingGrace = old }()
	if _, err := c.CleanLeftovers(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := leftover.List(); len(got) != 0 {
		t.Fatalf("kept after the grace period: %+v", got)
	}
}
