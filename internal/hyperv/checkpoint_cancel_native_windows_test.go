package hyperv

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// TestTakeCheckpointCancelNative cancels a backup while Hyper-V creates its
// checkpoint (on a running, disposable VM) and checks that no checkpoint is
// left behind.
func TestTakeCheckpointCancelNative(t *testing.T) {
	id := os.Getenv("BACKUPZIT_TEST_HYPERV_VM")
	if id == "" {
		t.Skip("BACKUPZIT_TEST_HYPERV_VM (GUID of a running, disposable VM) required")
	}
	vm, err := getVM(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	checkpoints := func() string {
		out, err := ps(context.Background(), `@(Get-VMSnapshot -VM (Get-VM -Id `+psq(id)+`) | ForEach-Object { $_.Id.Guid }) -join ','`)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	before := checkpoints()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Creating a recovery checkpoint takes several seconds (VSS in the guest).
	time.AfterFunc(3*time.Second, cancel)
	start := time.Now()
	_, _, _, warns, err := takeCheckpoint(ctx, *vm, snapPrefix+"canceltest", false, false)
	t.Logf("takeCheckpoint returned after %v: err=%v warns=%q", time.Since(start).Round(time.Millisecond), err, warns)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled (was the checkpoint created within 3 s?)", err)
	}
	// A creation job that kept running in Hyper-V after an interrupted
	// script would add its checkpoint only now.
	time.Sleep(30 * time.Second)
	if after := checkpoints(); after != before {
		t.Errorf("checkpoints before %q, after %q: the cancelled backup left one behind", before, after)
	}
}
