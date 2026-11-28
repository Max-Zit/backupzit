package imaging

import (
	"fmt"
	"time"
)

// SameIDOfflineWarning is FinishLayout's warning for a partition that
// became larger but got no volume to extend because another disk in the
// computer has the disk ID of the restored one, usually the backed up disk
// itself: Windows keeps the copy offline then (#61). Brought online next
// to the old disk it would get new IDs and no longer start (see
// RestoreOptions.KeepOffline), so the old disk goes first. The arguments
// are the partition number and the other disk; the recovery program
// translates the warning by this format.
const SameIDOfflineWarning = "partition %d: not extended, because disk %d in this computer has the same disk and partition IDs (the backed up disk), so Windows keeps this copy offline; remove the old disk, then extend the file system in Disk Management once this disk is online"

// noVolumeWarning is the warning for a partition at offset off of the
// restored disk ref (part as listed, nil if not) that became larger but has
// no volume to extend. disks are all disks as listed after the restore.
func noVolumeWarning(disks []Disk, ref DiskRef, part *Partition, off uint64) string {
	if part != nil && ref.ID != nil {
		if n, ok := DiskWithID(disks, ref.Number, ref.ID.Style, ref.ID.GPTDiskID, ref.ID.MBRSignature); ok {
			return fmt.Sprintf(SameIDOfflineWarning, part.Number, n)
		}
	}
	return fmt.Sprintf("partition at %d: no volume to extend; extend it in Disk Management", off)
}

// RestoreOptions configure an image restore.
type RestoreOptions struct {
	// TargetDisk is the disk number to overwrite. All data on it is lost.
	TargetDisk int
	// Image selects the disk image within the snapshot (default 0).
	Image int
	// Progress is called with bytes written and total bytes to write.
	Progress func(done, total uint64)
	// KeepOffline leaves the target disk offline afterwards. Use it when
	// the source disk is attached to the same machine: Windows would
	// otherwise assign new disk and partition IDs to resolve the duplicate
	// and the restored system would no longer boot.
	KeepOffline bool
	// Partitions limits the restore to these partition numbers of the
	// image; the others are created empty. Nil restores every backed up
	// partition.
	Partitions []int
	// Layout places the partitions differently from the image (moved,
	// larger, left out). Nil keeps the image's layout.
	Layout *Layout
	// Identity, when set, is the disk the restore was confirmed for: a
	// disk with the same number that is another one is refused.
	Identity *DiskIdentity
}

// RestoreStats summarises an image restore.
type RestoreStats struct {
	Partitions   int
	BytesWritten uint64
	Duration     time.Duration
	// Moved is set when partitions changed place or size: the boot files
	// need rebuilding (BCD on MBR disks refers to partition offsets).
	Moved bool `json:"moved,omitempty"`
	// Grown lists the target offsets of restored partitions that became
	// larger; their file systems are extended by FinishLayout.
	Grown []uint64 `json:"grown,omitempty"`
	// NewPartitionID is set when a partition restore had to give the
	// partition a new GPT ID because its own is in use in this computer.
	NewPartitionID string `json:"new_partition_id,omitempty"`
	// CreatedEFI lists the offsets of EFI system partitions the restore
	// created (MBR to GPT); FinishLayout formats them.
	CreatedEFI []uint64 `json:"created_efi,omitempty"`
	// NeedMBRCode is set when a GPT image went onto an MBR disk: the
	// Windows MBR boot code is written by FinishLayout.
	NeedMBRCode bool `json:"need_mbr_code,omitempty"`
	// Shrunk lists the NTFS partitions that were restored smaller.
	Shrunk []ShrinkReport `json:"shrunk,omitempty"`
	// Table is the identity of the target disk with the partition table
	// the restore wrote; the steps after the restore check it.
	Table *DiskIdentity `json:"-"`
	// PartitionOffset is where a partition restore wrote the partition.
	PartitionOffset uint64 `json:"partition_offset,omitempty"`
}

// ShrinkReport describes one shrunk partition. Windows checks the file
// system (chkdsk) once at the next start.
type ShrinkReport struct {
	Partition     int    `json:"partition"`
	MovedClusters uint64 `json:"moved_clusters"`
	// Orphaned clusters behind the new end belonged to no file.
	Orphaned uint64 `json:"orphaned,omitempty"`
	// ShadowCopiesRemoved is set when the volume had VSS shadow copies:
	// they refer to absolute places and are removed by the shrink.
	ShadowCopiesRemoved bool `json:"shadow_copies_removed,omitempty"`
}
