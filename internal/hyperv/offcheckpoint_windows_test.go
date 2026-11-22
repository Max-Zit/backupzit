package hyperv

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// 0.58: a VM that is off gets a crash-consistent recovery checkpoint (so
// its change tracking ID advances), never an application-consistent attempt
// or a production checkpoint, and no warnings when Hyper-V cannot take one.
func TestOffVMCheckpoint(t *testing.T) {
	vm := vmInfo{ID: "5a86f77b-3849-4ca5-b1d6-292ce1c9d647", Name: "hv-deb", State: "Off"}
	checkpoint := []byte(`{"type":"Recovery","state":"Off","disks":[{"type":"SCSI","num":0,"loc":0,"path":"V:\\vm\\scsi0-0.vhdx"}]}`)
	original := ps
	defer func() { ps = original }()

	var scripts []string
	fail := false
	ps = func(_ context.Context, script string) ([]byte, error) {
		scripts = append(scripts, script)
		if strings.Contains(script, "Checkpoint-VM") {
			t.Error("a production checkpoint was tried for a VM that is off")
		}
		if fail {
			return nil, errors.New("recovery checkpoint: The operation is not supported for a replica VM")
		}
		return checkpoint, nil
	}
	ctx := context.Background()
	ci, consistency, rct, warns, err := createCheckpoint(ctx, ctx, vm, "backupzit_off", false, true)
	if err != nil || !rct || len(ci.Disks) != 1 || consistency != "VM was off, disks read from a recovery checkpoint" || len(warns) != 0 {
		t.Fatalf("checkpoint: %+v %q rct=%v warns=%v err=%v", ci, consistency, rct, warns, err)
	}
	if len(scripts) != 1 || !strings.Contains(scripts[0], "$level = 2") {
		t.Fatalf("scripts: %d, first %q", len(scripts), scripts[0][:80])
	}

	scripts, fail = nil, true
	_, _, _, warns, err = createCheckpoint(ctx, ctx, vm, "backupzit_off", false, true)
	if err == nil || len(warns) != 0 || len(scripts) != 1 {
		t.Fatalf("failed checkpoint: err=%v warns=%v scripts=%d", err, warns, len(scripts))
	}

	// A running VM still tries application-consistent first.
	scripts, fail = nil, false
	vm.State = "Running"
	if _, c, _, _, err := createCheckpoint(ctx, ctx, vm, "backupzit_on", false, false); err != nil || !strings.HasPrefix(c, "recovery checkpoint, application-consistent") || !strings.Contains(scripts[0], "$level = 1") {
		t.Fatalf("running VM: %q %v", c, err)
	}
}

func TestAllDisksIn(t *testing.T) {
	d0 := vmDisk{Type: "SCSI", Number: 0, Location: 0, Path: `V:\vm\a.vhdx`}
	d1 := vmDisk{Type: "SCSI", Number: 0, Location: 1, Path: `V:\vm\b.vhds`}
	ci := checkpointInfo{Disks: []vmDisk{d0}}
	if allDisksIn(ci, []vmDisk{d0, d1}, map[string]string{}) {
		t.Error("a disk left out of the checkpoint was not noticed")
	}
	if !allDisksIn(ci, []vmDisk{d0, d1}, map[string]string{d1.key(): d1.Path}) {
		t.Error("an excluded disk counted as missing")
	}
}
