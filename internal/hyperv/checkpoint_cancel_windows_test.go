package hyperv

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A backup cancelled while Hyper-V creates its checkpoint must neither kill
// the creation (the checkpoint would keep Hyper-V's default name, which no
// cleanup finds) nor leave the finished checkpoint behind.
func TestTakeCheckpointCancel(t *testing.T) {
	const name = "backupzit_cancel"
	vm := vmInfo{ID: "5a86f77b-3849-4ca5-b1d6-292ce1c9d647"}
	checkpoint := []byte(`{"type":"Recovery","state":"Running","disks":[{"type":"SCSI","num":0,"loc":0,"path":"C:\\vm\\disk.avhdx"}]}`)

	for _, tc := range []struct {
		name          string
		cancelBefore  bool // backup cancelled before the checkpoint starts
		cancelDuring  bool // cancelled while the creation script runs
		removeFails   bool
		wantErr       error
		wantCreates   int
		wantRemoves   int
		wantWarnCount int
	}{
		{name: "not cancelled", wantCreates: 1},
		{name: "cancelled during creation", cancelDuring: true, wantErr: context.Canceled, wantCreates: 1, wantRemoves: 1},
		{name: "cancelled during creation, removal fails", cancelDuring: true, removeFails: true, wantErr: context.Canceled, wantCreates: 1, wantRemoves: 1, wantWarnCount: 1},
		{name: "cancelled before", cancelBefore: true, wantErr: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelBefore {
				cancel()
			}
			original := ps
			defer func() { ps = original }()
			creates, removes := 0, 0
			exists := false
			ps = func(run context.Context, script string) ([]byte, error) {
				if strings.Contains(script, ").Count") { // checkpointExists (0.56)
					if exists {
						return []byte("1"), nil
					}
					return []byte("0"), nil
				}
				if run.Err() != nil {
					t.Error("PowerShell started with a cancelled context: the checkpoint would be left half done")
				}
				if !strings.Contains(script, "CreateSnapshot") && strings.Contains(script, "Remove-VMSnapshot") {
					removes++
					if !strings.Contains(script, psq(vm.ID)) || !strings.Contains(script, psq(name)) {
						t.Error("removal does not select the VM and the checkpoint name")
					}
					if tc.removeFails {
						return nil, errors.New("injected removal failure")
					}
					exists = false
					return nil, nil
				}
				creates++
				exists = true
				if !strings.Contains(script, psq(name)) {
					t.Error("creation script does not name the checkpoint")
				}
				if tc.cancelDuring {
					cancel()
					if run.Err() != nil {
						t.Error("cancelling the backup interrupted the checkpoint creation")
					}
				}
				return checkpoint, nil
			}

			ci, _, rct, warns, err := takeCheckpoint(ctx, vm, name, false, false)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if creates != tc.wantCreates || removes != tc.wantRemoves {
				t.Errorf("creates = %d, removes = %d, want %d and %d", creates, removes, tc.wantCreates, tc.wantRemoves)
			}
			if len(warns) != tc.wantWarnCount {
				t.Errorf("warnings = %q, want %d", warns, tc.wantWarnCount)
			}
			if tc.wantErr == nil && (len(ci.Disks) != 1 || !rct) {
				t.Errorf("checkpoint = %+v rct=%v, want one disk with change tracking", ci, rct)
			}
			if tc.wantErr != nil && len(ci.Disks) != 0 {
				t.Errorf("disks %+v returned with an error", ci.Disks)
			}
		})
	}
}

// The wait for a checkpoint is bounded only once the backup is cancelled:
// without a cancel a slow (application-consistent) checkpoint runs to the
// end; after a cancel it gets checkpointGrace, then the run reports that
// Hyper-V may still add one under its default name.
func TestTakeCheckpointGrace(t *testing.T) {
	const name = "backupzit_grace"
	vm := vmInfo{ID: "5a86f77b-3849-4ca5-b1d6-292ce1c9d647"}
	checkpoint := []byte(`{"type":"Recovery","state":"Running","disks":[{"type":"SCSI","num":0,"loc":0,"path":"C:\\vm\\disk.avhdx"}]}`)
	grace := checkpointGrace
	checkpointGrace = 50 * time.Millisecond
	original := ps
	t.Cleanup(func() { checkpointGrace, ps = grace, original })

	// Slower than the grace, not cancelled: succeeds.
	ps = func(run context.Context, script string) ([]byte, error) {
		select {
		case <-time.After(200 * time.Millisecond):
			return checkpoint, nil
		case <-run.Done():
			return nil, run.Err()
		}
	}
	if ci, _, _, _, err := takeCheckpoint(context.Background(), vm, name, false, false); err != nil || len(ci.Disks) != 1 {
		t.Fatalf("slow checkpoint without a cancel: %v %+v", err, ci)
	}

	// Cancelled, and creating takes longer than the grace: cut off with a
	// warning that names the possible leftover.
	ctx, cancel := context.WithCancel(context.Background())
	ps = func(run context.Context, script string) ([]byte, error) {
		cancel()
		select {
		case <-time.After(5 * time.Second):
			return checkpoint, nil
		case <-run.Done():
			return nil, run.Err()
		}
	}
	start := time.Now()
	_, _, _, warns, err := takeCheckpoint(ctx, vm, name, true, false)
	if !errors.Is(err, context.Canceled) && !strings.Contains(fmt.Sprint(err), "canceled") {
		t.Fatalf("err = %v, want the cancel", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("took %v: the grace did not bound the wait", time.Since(start))
	}
	if len(warns) == 0 || !strings.Contains(warns[len(warns)-1], "did not finish within") {
		t.Errorf("warnings %q lack the possible leftover", warns)
	}
}
