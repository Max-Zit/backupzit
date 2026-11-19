package hyperv

import (
	"context"
	"errors"
	"strings"
	"testing"
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
			ps = func(run context.Context, script string) ([]byte, error) {
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
					return nil, nil
				}
				creates++
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

			ci, _, rct, warns, err := takeCheckpoint(ctx, vm, name, false)
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
