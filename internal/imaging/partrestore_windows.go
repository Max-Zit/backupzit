//go:build windows

package imaging

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/imaging/ntfsresize"
	"github.com/max-zit/backupzit/internal/repo"
	"golang.org/x/sys/windows"
)

// RestorePartition writes one partition of a disk image to a disk whose
// other partitions stay untouched: over an existing partition (Replace)
// or into free space. The partition keeps its GPT ID from the image
// unless another partition in this computer already has it.
func RestorePartition(ctx context.Context, r *repo.Repository, sn *repo.Snapshot, opts PartitionRestoreOptions) (*RestoreStats, error) {
	if opts.Image < 0 || opts.Image >= len(sn.Images) {
		return nil, fmt.Errorf("snapshot %s has no disk image %d", sn.ID.Short(), opts.Image)
	}
	img := &sn.Images[opts.Image]
	start := time.Now()
	disks, err := ListDisks()
	if err != nil {
		return nil, err
	}
	var target *Disk
	for i := range disks {
		if disks[i].Number == opts.TargetDisk {
			target = &disks[i]
		}
	}
	if target == nil {
		return nil, fmt.Errorf("target disk %d not found", opts.TargetDisk)
	}
	w, err := planPartitionRestore(img, target, opts)
	if err != nil {
		return nil, err
	}
	var old *Partition
	for i := range target.Partitions {
		if target.Partitions[i].Number == opts.Replace {
			old = &target.Partitions[i]
		}
	}
	if old != nil && target.System {
		for _, m := range old.MountPoints {
			if strings.EqualFold(strings.TrimRight(m, `\`), os.Getenv("SystemDrive")) {
				return nil, fmt.Errorf("partition %d holds the running Windows", opts.Replace)
			}
		}
	}
	var sp *ntfsresize.Plan
	if w.Shrink {
		if sp, err = planShrink(ctx, r, w); err != nil {
			return nil, err
		}
	}
	st := &RestoreStats{}
	partID := w.Src.GPTID
	if target.Style == StyleGPT && (partID == "" || partitionIDInUse(disks, partID, target.Number, opts.Replace)) {
		g, err := windows.GenerateGUID()
		if err != nil {
			return nil, err
		}
		partID = strings.Trim(g.String(), "{}")
		st.NewPartitionID = partID
	}

	// Lock and dismount the partition that is overwritten; its sectors can
	// then be written through the disk. Free space can be written anyway.
	if old != nil && old.VolumeGUIDPath != "" {
		h, err := lockVolume(old.VolumeGUIDPath)
		if err != nil {
			return nil, fmt.Errorf("partition %d is in use and cannot be locked (close programs using it): %w", opts.Replace, err)
		}
		defer windows.CloseHandle(h)
	}
	dh, err := openDevice(physicalDrivePath(target.Number), true)
	if err != nil {
		return nil, fmt.Errorf("open target disk: %w", err)
	}
	defer windows.CloseHandle(dh)
	f := os.NewFile(uintptr(dh), physicalDrivePath(target.Number))

	// Data first, then the partition table entry: Windows mounts the new
	// partition only when its contents are complete.
	err = writePartition(ctx, r, f, w, sp, img.SectorSize, st, func(n uint64) {
		if opts.Progress != nil {
			opts.Progress(st.BytesWritten, w.Src.StoredBytes)
		}
	})
	if err != nil {
		return st, err
	}
	raw, err := ioctl(dh, ioctlDiskGetDriveLayoutEx, nil, 4096)
	if err != nil {
		return st, fmt.Errorf("read partition table: %w", err)
	}
	lay, err := setLayoutEntry(raw, w, opts.Replace, partID, target.SectorSize)
	if err != nil {
		return st, err
	}
	if _, err := ioctl(dh, ioctlDiskSetDriveLayoutEx, lay, 0); err != nil {
		return st, fmt.Errorf("write partition table: %w", err)
	}
	if err := windows.FlushFileBuffers(dh); err != nil {
		return st, fmt.Errorf("flush: %w", err)
	}
	_, _ = ioctl(dh, ioctlDiskUpdateProperties, nil, 0)
	st.Moved = true // the BCD refers to the partition: rebuild the boot files
	st.Duration = time.Since(start)
	return st, nil
}
