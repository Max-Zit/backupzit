package hyperv

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestTakeCheckpointCancelNative cancels a backup while Hyper-V creates its
// checkpoint (on a running, disposable VM with a guest OS, so that creating
// takes a few seconds) and checks that no checkpoint is left behind.
func TestTakeCheckpointCancelNative(t *testing.T) {
	id := os.Getenv("BACKUPZIT_TEST_HYPERV_CANCEL_VM")
	if id == "" {
		t.Skip("BACKUPZIT_TEST_HYPERV_CANCEL_VM (GUID of a running, disposable VM with a guest OS) required")
	}
	if !regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`).MatchString(id) {
		t.Fatalf("BACKUPZIT_TEST_HYPERV_CANCEL_VM %q is not a VM GUID", id)
	}
	vm, err := getVM(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	checkpoints := func() []string {
		out, err := ps(context.Background(), `@(Get-VMSnapshot -VM (Get-VM -Id `+psq(id)+`) | ForEach-Object { $_.Id.Guid }) -join ','`)
		if err != nil {
			t.Fatal(err)
		}
		if s := strings.TrimSpace(string(out)); s != "" {
			return strings.Split(s, ",")
		}
		return nil
	}
	before := checkpoints()
	added := func() (ids []string) {
		for _, c := range checkpoints() {
			if !strings.Contains(strings.Join(before, ","), c) {
				ids = append(ids, c)
			}
		}
		return ids
	}
	t.Cleanup(func() { // whatever the test left on the VM goes again
		for _, c := range added() {
			_, _ = ps(context.Background(), `Get-VMSnapshot -VM (Get-VM -Id `+psq(id)+`) | Where-Object { $_.Id.Guid -eq `+psq(c)+` } | Remove-VMSnapshot`)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel one second after the first script started: creating a recovery
	// checkpoint takes several seconds (VSS in the guest).
	orig := ps
	var once sync.Once
	ps = func(c context.Context, script string) ([]byte, error) {
		once.Do(func() { time.AfterFunc(time.Second, cancel) })
		return orig(c, script)
	}
	start := time.Now()
	_, _, _, warns, err := takeCheckpoint(ctx, *vm, snapPrefix+"canceltest", false, false)
	ps = orig
	t.Logf("takeCheckpoint returned after %v: err=%v warns=%q", time.Since(start).Round(time.Millisecond), err, warns)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled (was the checkpoint created within a second?)", err)
	}
	// A creation job that kept running in Hyper-V after an interrupted
	// script would add its checkpoint only now.
	time.Sleep(30 * time.Second)
	if a := added(); len(a) > 0 {
		t.Errorf("the cancelled backup left checkpoints %v behind", a)
	}
}
