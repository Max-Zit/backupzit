package hyperv

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/diskimg"
	"github.com/max-zit/backupzit/internal/repo"
)

type testChangeTracker struct {
	on                 bool
	id                 string
	stateErr, queryErr error
	ranges             [][2]uint64
	stateCalls         int
	queried            []string
}

func (s *testChangeTracker) rctState() (bool, string, error) {
	s.stateCalls++
	return s.on, s.id, s.stateErr
}

func (s *testChangeTracker) rctChanges(id string) ([][2]uint64, error) {
	s.queried = append(s.queried, id)
	return s.ranges, s.queryErr
}

func TestRCTReadPlan(t *testing.T) {
	const size = 3*diskimg.BlockSize + 512
	apiErr := errors.New("tracking position no longer available")
	for _, scenario := range []struct {
		name, reason         string
		rct, previous, image bool
		previousSize         uint64
		previousID           string
		on                   bool
		stateErr, queryErr   error
		ranges               [][2]uint64
		incremental          bool
		queries              int
	}{
		{name: "not-requested", reason: "rct_not_requested"},
		{name: "state-error", rct: true, stateErr: apiErr, reason: "rct_state_failed"},
		{name: "disabled", rct: true, reason: "rct_disabled"},
		{name: "first-backup", rct: true, on: true, reason: "no_previous_backup"},
		{name: "missing-image", rct: true, on: true, previous: true, reason: "no_previous_backup"},
		{name: "grown", rct: true, on: true, previous: true, image: true, previousSize: size - 512, previousID: "old-id", reason: "disk_size_changed"},
		{name: "shrunk", rct: true, on: true, previous: true, image: true, previousSize: size + 512, previousID: "old-id", reason: "disk_size_changed"},
		{name: "missing-id", rct: true, on: true, previous: true, image: true, previousSize: size, reason: "previous_id_missing"},
		{name: "invalid-id", rct: true, on: true, previous: true, image: true, previousSize: size, previousID: "old-id", queryErr: apiErr, queries: 1, reason: "rct_query_failed"},
		{name: "partial-query-error", rct: true, on: true, previous: true, image: true, previousSize: size, previousID: "old-id", queryErr: apiErr, ranges: [][2]uint64{{diskimg.BlockSize, 4}}, queries: 1, reason: "rct_query_failed"},
		{name: "changed", rct: true, on: true, previous: true, image: true, previousSize: size, previousID: "old-id", ranges: [][2]uint64{{diskimg.BlockSize, 4}}, queries: 1, incremental: true, reason: "rct_changes"},
		{name: "unchanged", rct: true, on: true, previous: true, image: true, previousSize: size, previousID: "old-id", queries: 1, incremental: true, reason: "rct_changes"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := context.Background()
			be, err := backend.OpenLocal(filepath.Join(t.TempDir(), "repo"))
			if err != nil {
				t.Fatal(err)
			}
			r, err := repo.Init(ctx, be)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			old := bytes.Repeat([]byte{0x41}, size)
			img, err := diskimg.Store(ctx, r, bytes.NewReader(old), size, 0, "previous", "checkpoint", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Flush(ctx); err != nil {
				t.Fatal(err)
			}
			var previous *diskimg.Previous
			if scenario.previous {
				previous = &diskimg.Previous{ChangeID: scenario.previousID}
				if scenario.image {
					previous.Image = &img
					img.Size = scenario.previousSize
				}
			}
			tracker := &testChangeTracker{on: scenario.on, id: "new-id", stateErr: scenario.stateErr, queryErr: scenario.queryErr, ranges: scenario.ranges}
			plan := planDiskRead(size, scenario.rct, previous, tracker)
			if plan.reason != scenario.reason || (plan.base != nil) != scenario.incremental {
				t.Fatalf("unexpected decision: %+v", plan)
			}
			if len(tracker.queried) != scenario.queries || tracker.stateCalls != boolInt(scenario.rct) {
				t.Fatalf("unexpected API calls: %+v", tracker)
			}
			if scenario.queries > 0 && tracker.queried[0] != "old-id" {
				t.Fatal(tracker.queried)
			}
			wantID := ""
			if scenario.rct && scenario.on && scenario.stateErr == nil {
				wantID = "new-id"
			}
			if plan.changeID != wantID {
				t.Fatalf("tracking ID lost: %q", plan.changeID)
			}
			if !scenario.incremental && !reflect.DeepEqual(plan.areas, []diskimg.Area{{Length: size}}) {
				t.Fatalf("fallback must read the entire disk, not partial RCT results: %v", plan.areas)
			}
			var fields map[string]any
			plan.log(func(message string, kv ...any) {
				if message != "hyperv disk read" {
					t.Fatal(message)
				}
				fields = map[string]any{}
				for i := 0; i < len(kv); i += 2 {
					fields[fmt.Sprint(kv[i])] = kv[i+1]
				}
			}, "vm-guid", "scsi0:0", size)
			if fields["reason"] != scenario.reason || fields["vm_id"] != "vm-guid" || fields["disk"] != "scsi0:0" || fields["disk_bytes"] != uint64(size) || fields["mode"] != plan.mode() {
				t.Fatalf("missing structured diagnostic: %v", fields)
			}
			if scenario.stateErr != nil || scenario.queryErr != nil {
				if fields["error"] != apiErr.Error() || !errors.Is(plan.err, apiErr) {
					t.Fatalf("underlying API error lost: %v", fields)
				}
			} else if _, ok := fields["error"]; ok {
				t.Fatalf("spurious API error: %v", fields)
			}
			plan.log(nil, "vm-guid", "scsi0:0", size)
			current := append([]byte(nil), old...)
			if scenario.name != "unchanged" {
				copy(current[diskimg.BlockSize:2*diskimg.BlockSize], bytes.Repeat([]byte{0x42}, diskimg.BlockSize))
			}
			var read uint64
			stored, err := diskimg.StoreAreas(ctx, r, bytes.NewReader(current), size, plan.areas, plan.base, 0, "current", "checkpoint", func(n uint64) { read += n })
			if err != nil {
				t.Fatal(err)
			}
			wantRead := uint64(size)
			if scenario.incremental {
				wantRead = diskimg.BlockSize
				if scenario.name == "unchanged" {
					wantRead = 0
				}
			}
			if read != wantRead {
				t.Fatalf("read %d bytes, expected %d", read, wantRead)
			}
			if err := r.Flush(ctx); err != nil {
				t.Fatal(err)
			}
			f, err := os.Create(filepath.Join(t.TempDir(), "restored.raw"))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if err := diskimg.Write(ctx, r, &stored, f, true, nil); err != nil {
				t.Fatal(err)
			}
			got := make([]byte, size)
			if _, err := f.ReadAt(got, 0); err != nil || !bytes.Equal(got, current) {
				t.Fatalf("reconstructed disk differs: %v", err)
			}
		})
	}
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
