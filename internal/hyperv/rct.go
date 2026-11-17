package hyperv

import (
	"github.com/max-zit/backupzit/internal/diskimg"
	"github.com/max-zit/backupzit/internal/repo"
)

// diskChangeTracker is implemented by the Windows Virtual Disk API adapter.
// Keeping selection independent of that API lets CI exercise every fallback.
type diskChangeTracker interface {
	rctState() (bool, string, error)
	rctChanges(string) ([][2]uint64, error)
}

type diskReadPlan struct {
	areas    []diskimg.Area
	base     *repo.DiskImage
	method   string
	changeID string
	reason   string
	err      error
}

// planDiskRead preserves the conservative whole-disk fallback. Its reason
// explains the decision; an RCT failure is not a backup failure.
func planDiskRead(size uint64, rct bool, previous *diskimg.Previous, tracker diskChangeTracker) diskReadPlan {
	p := diskReadPlan{areas: []diskimg.Area{{Length: size}}, method: "whole disk", reason: "rct_not_requested"}
	if !rct {
		return p
	}
	on, id, err := tracker.rctState()
	if err != nil {
		p.reason, p.err = "rct_state_failed", err
		return p
	}
	if !on {
		p.reason = "rct_disabled"
		return p
	}
	p.changeID = id
	switch {
	case previous == nil || previous.Image == nil:
		p.reason = "no_previous_backup"
		return p
	case previous.Image.Size != size:
		p.reason = "disk_size_changed"
		return p
	case previous.ChangeID == "":
		p.reason = "previous_id_missing"
		return p
	}
	ranges, err := tracker.rctChanges(previous.ChangeID)
	if err != nil {
		p.reason, p.err = "rct_query_failed", err
		return p
	}
	p.areas = make([]diskimg.Area, 0, len(ranges))
	for _, rg := range ranges {
		p.areas = append(p.areas, diskimg.Area{Offset: rg[0], Length: rg[1]})
	}
	p.base, p.method, p.reason = previous.Image, "changed blocks", "rct_changes"
	return p
}

func (p diskReadPlan) mode() string {
	if p.base != nil {
		return "incremental"
	}
	return "full"
}

func (p diskReadPlan) log(log func(string, ...any), vmID, disk string, size uint64) {
	if log == nil {
		return
	}
	kv := []any{"vm_id", vmID, "disk", disk, "mode", p.mode(), "reason", p.reason, "disk_bytes", size}
	if p.err != nil {
		kv = append(kv, "error", p.err.Error())
	}
	log("hyperv disk read", kv...)
}
