package imaging

import "time"

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
}

// ShrinkReport describes one shrunk partition. Windows checks the file
// system (chkdsk) once at the next start.
type ShrinkReport struct {
	Partition     int    `json:"partition"`
	MovedClusters uint64 `json:"moved_clusters"`
	// Orphaned clusters behind the new end belonged to no file.
	Orphaned uint64 `json:"orphaned,omitempty"`
}
