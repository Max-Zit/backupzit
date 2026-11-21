package agent

import (
	"context"
	"testing"

	"github.com/max-zit/backupzit/internal/api"
)

// #44: the progress of a VM backup names the VM it is on and lists the
// VMs finished, with their result; a finished run reports nothing more.
func TestVMProgress(t *testing.T) {
	a := &Agent{}
	ctx := withRunID(context.Background(), 7)
	a.runStarted(api.Run{ID: 7, Kind: api.KindVMBackup})
	a.vmStarted(ctx, "db", 3)
	a.vmFinished(ctx, "db", true)
	a.vmStarted(ctx, "web", 3)
	p := a.runProgress()
	if len(p) != 1 || p[0].VM != "web" || p[0].VMsDone != 1 || p[0].VMsTotal != 3 || len(p[0].VMsFinished) != 1 || !p[0].VMsFinished[0].Failed {
		t.Fatalf("progress %+v", p)
	}
	a.vmFinished(ctx, "web", false)
	if p := a.runProgress(); p[0].VM != "" || p[0].VMsDone != 2 || p[0].VMsFinished[1].Failed {
		t.Fatalf("after web: %+v", p)
	}
	a.vmFinished(withRunID(context.Background(), 8), "other", false) // another run: ignored
	a.runFinished(7, api.RunResult{Status: api.StatusSuccess})
	if p := a.runProgress(); len(p) != 0 {
		t.Fatalf("finished run still reports %+v", p)
	}
}
