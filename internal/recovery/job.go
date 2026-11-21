package recovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/max-zit/backupzit/internal/imaging"
	"github.com/max-zit/backupzit/internal/repo"
)

// Job is one image restore.
type Job struct {
	Snapshot *repo.Snapshot
	// Image is the index of the backed up disk in Snapshot.Images.
	Image int
	// Partitions to restore; nil restores every backed up partition.
	Partitions []int
	// Layout places the partitions on the target disk (moved, resized,
	// left out); nil keeps the image's layout. With a layout, Partitions
	// is not used: the layout says which partitions are restored.
	Layout *imaging.Layout
	// Into restores a single partition onto a disk whose other partitions
	// stay (partition restore); Partitions and Layout are then not used.
	Into       *PartitionTarget
	TargetDisk int
	// NewHardware prepares the restored Windows for this machine; drivers
	// from DriverDirs and the recovery medium are added.
	NewHardware bool
	DriverDirs  []string
	KeepOffline bool
	// Verify reads every block that will be written before the target
	// disk is touched.
	Verify bool
}

// PartitionTarget says where a partition restore writes: over partition
// Replace of the target disk, or into free space (Replace 0, at Offset or
// the first space that fits). Length 0 keeps the size of the replaced
// partition or of the image partition.
type PartitionTarget struct {
	Partition int    `json:"partition"` // number in the image
	Replace   int    `json:"replace,omitempty"`
	Offset    uint64 `json:"offset,omitempty"`
	Length    uint64 `json:"length,omitempty"`
}

// ProtectedDisks returns the numbers of the disks that hold any of paths
// (the recovery medium, a local repository): they must not be restored to.
func ProtectedDisks(disks []imaging.Disk, paths []string) map[int]string {
	out := map[int]string{}
	for _, p := range paths {
		vol := strings.ToUpper(filepath.VolumeName(p))
		if len(vol) != 2 || vol[1] != ':' {
			continue
		}
		for _, d := range disks {
			for _, part := range d.Partitions {
				for _, m := range part.MountPoints {
					if strings.EqualFold(strings.TrimRight(m, `\`), vol) {
						out[d.Number] = p
					}
				}
			}
		}
	}
	return out
}

// Check validates the job against the local disks. protected maps disk
// numbers that must not be overwritten to the reason.
func (j Job) Check(disks []imaging.Disk, protected map[int]string) error {
	if j.Snapshot == nil {
		return errors.New("no snapshot selected")
	}
	if j.Image < 0 || j.Image >= len(j.Snapshot.Images) {
		return fmt.Errorf("snapshot %s has no disk image %d", j.Snapshot.ID.Short(), j.Image)
	}
	img := &j.Snapshot.Images[j.Image]
	var target *imaging.Disk
	for i := range disks {
		if disks[i].Number == j.TargetDisk {
			target = &disks[i]
		}
	}
	if target == nil {
		return fmt.Errorf("target disk %d not found", j.TargetDisk)
	}
	if target.System {
		return fmt.Errorf("disk %d holds the running Windows", j.TargetDisk)
	}
	if why, ok := protected[j.TargetDisk]; ok {
		return fmt.Errorf("disk %d holds %s and cannot be the target", j.TargetDisk, why)
	}
	if target.SectorSize != img.SectorSize {
		return fmt.Errorf("target disk has %d byte sectors, image was taken from %d byte sectors", target.SectorSize, img.SectorSize)
	}
	if j.Into != nil {
		if j.Layout != nil || j.Partitions != nil {
			return errors.New("a partition restore takes neither a layout nor a partition list")
		}
		return imaging.CheckPartitionRestore(img, target, j.partitionOptions())
	}
	if j.Partitions != nil && len(j.Partitions) == 0 {
		return errors.New("no partition selected")
	}
	if j.Layout != nil {
		if j.Partitions != nil {
			return errors.New("give the partitions to restore in the layout")
		}
		if j.Layout.DiskSize > target.Size {
			return fmt.Errorf("the layout was planned for a disk of %d bytes, disk %d has %d", j.Layout.DiskSize, j.TargetDisk, target.Size)
		}
		restored := 0
		for _, lp := range j.Layout.Partitions {
			if lp.Restore {
				restored++
			}
		}
		if restored == 0 {
			return errors.New("no partition selected")
		}
		// Limits were checked when the layout was made; check the shape.
		if err := j.Layout.Validate(img, unlimited(img)); err != nil {
			return err
		}
		return nil
	}
	for _, n := range j.Partitions {
		var found *repo.PartitionImage
		for i := range img.Partitions {
			if img.Partitions[i].Number == n {
				found = &img.Partitions[i]
			}
		}
		if found == nil {
			return fmt.Errorf("the image has no partition %d", n)
		}
		if !found.Included {
			return fmt.Errorf("partition %d was not backed up", n)
		}
	}
	var needed uint64
	for _, p := range img.Partitions {
		needed = max(needed, p.Offset+p.Length)
	}
	if _, end := imaging.UsableRange(img.Style, target.Size, target.SectorSize); end < needed {
		return fmt.Errorf("target disk %d is too small: %d bytes, the image needs %d", j.TargetDisk, target.Size, needed+(target.Size-end))
	}
	return nil
}

// unlimited allows any size for every partition, for checking the shape
// of a layout (order, overlap, disk size) without resize limits.
func unlimited(img *repo.DiskImage) map[int]imaging.PartitionLimits {
	m := map[int]imaging.PartitionLimits{}
	for _, p := range img.Partitions {
		m[p.Number] = imaging.PartitionLimits{Resizable: true, MinSize: 1}
	}
	return m
}

func (j Job) partitionOptions() imaging.PartitionRestoreOptions {
	return imaging.PartitionRestoreOptions{Image: j.Image, Partition: j.Into.Partition, TargetDisk: j.TargetDisk,
		Replace: j.Into.Replace, Offset: j.Into.Offset, Length: j.Into.Length}
}

// restoredPartitions returns the numbers of the partitions the job
// writes (nil: every backed up one).
func (j Job) restoredPartitions() []int {
	if j.Into != nil {
		return []int{j.Into.Partition}
	}
	if j.Layout == nil {
		return j.Partitions
	}
	out := []int{}
	for _, lp := range j.Layout.Partitions {
		if lp.Restore {
			out = append(out, lp.Source)
		}
	}
	return out
}

func imagePartition(img *repo.DiskImage, num int) *repo.PartitionImage {
	for i := range img.Partitions {
		if img.Partitions[i].Number == num {
			return &img.Partitions[i]
		}
	}
	return &repo.PartitionImage{Number: num}
}

// SameDiskAsSource tells whether the target disk looks like the disk the
// image was taken from (same partition table ID). Restoring over it
// destroys the last copy if the backup turns out damaged, so the user
// should verify the snapshot first.
func SameDiskAsSource(img *repo.DiskImage, target imaging.Disk) bool {
	switch {
	case img.Style == imaging.StyleGPT && target.Style == imaging.StyleGPT:
		return img.GPTDiskID != "" && strings.EqualFold(img.GPTDiskID, target.GPTDiskID)
	case img.Style == imaging.StyleMBR && target.Style == imaging.StyleMBR:
		return img.MBRSignature != 0 && img.MBRSignature == target.MBRSignature
	}
	return false
}

// Progress reports a phase ("verify", "write", "hardware") and bytes.
type Progress func(phase string, done, total uint64)

// Result of a job.
type Result struct {
	Restore  *imaging.RestoreStats
	Hardware *imaging.HardwareReport
	Boot     *imaging.BootReport
	Warnings []string
}

// Logger writes the protocol of a job: to a file (on the recovery medium)
// and to a callback for the user interface.
type Logger struct {
	mu   sync.Mutex
	w    io.Writer
	call func(string)
}

// NewLogger writes to w (may be nil) and calls fn (may be nil) per line.
func NewLogger(w io.Writer, fn func(string)) *Logger { return &Logger{w: w, call: fn} }

// Printf logs one line with a timestamp.
func (l *Logger) Printf(format string, args ...any) {
	if l == nil {
		return
	}
	line := fmt.Sprintf(format, args...)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.w != nil {
		_, _ = fmt.Fprintf(l.w, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), line)
	}
	if l.call != nil {
		l.call(line)
	}
}

// Verify reads every block of the partitions the job writes and checks it
// (blobs are authenticated when loaded). It writes nothing.
func Verify(ctx context.Context, r *repo.Repository, j Job, progress Progress) error {
	img := &j.Snapshot.Images[j.Image]
	only := j.restoredPartitions()
	sel := map[int]bool{}
	for _, n := range only {
		sel[n] = true
	}
	var total, done uint64
	var parts []*repo.PartitionImage
	for i := range img.Partitions {
		p := &img.Partitions[i]
		if p.Included && (only == nil || sel[p.Number]) {
			parts = append(parts, p)
			total += p.StoredBytes
		}
	}
	for _, p := range parts {
		ids, err := r.LoadBlockMap(ctx, p)
		if err != nil {
			return fmt.Errorf("partition %d: %w", p.Number, err)
		}
		for b, id := range ids {
			if err := ctx.Err(); err != nil {
				return err
			}
			if id.IsNull() {
				continue
			}
			data, err := r.LoadBlob(ctx, repo.DataBlob, id)
			if err != nil {
				return fmt.Errorf("partition %d block %d: %w", p.Number, b, err)
			}
			done += uint64(len(data))
			if progress != nil {
				progress("verify", done, total)
			}
		}
	}
	return nil
}

// Run executes the job: optional verification, the image restore and the
// preparation for new hardware.
func Run(ctx context.Context, s *Session, j Job, progress Progress, log *Logger) (*Result, error) {
	img := &j.Snapshot.Images[j.Image]
	log.Printf("image restore: snapshot %s from %s, backed up disk %d (%s) to disk %d",
		j.Snapshot.ID.Short(), j.Snapshot.Time.Local().Format("2006-01-02 15:04"), img.Number, strings.ToUpper(img.Style), j.TargetDisk)
	if only := j.restoredPartitions(); only != nil {
		log.Printf("partitions: %v", only)
	}
	if j.Layout != nil {
		for _, lp := range j.Layout.Partitions {
			log.Printf("layout: partition %d at %d, %d bytes, restore %v", lp.Source, lp.Offset, lp.Length, lp.Restore)
		}
	}
	// A shared lock keeps prune from deleting data during the restore. A
	// read-only view (immutable storage as of a past time) cannot take
	// one; nothing can delete from it either.
	if lock, err := s.Repo.Lock(ctx, false, time.Minute); err != nil {
		log.Printf("repository not locked (%v); continuing", err)
	} else {
		defer func() { _ = lock.Unlock() }()
	}

	if j.Verify {
		log.Printf("verifying the snapshot")
		if err := Verify(ctx, s.Repo, j, progress); err != nil {
			log.Printf("verification failed: %v", err)
			return nil, fmt.Errorf("the snapshot is damaged, nothing was written: %w", err)
		}
		log.Printf("snapshot verified")
	}
	res := &Result{}
	var p func(done, total uint64)
	if progress != nil {
		p = func(done, total uint64) { progress("write", done, total) }
	}
	var st *imaging.RestoreStats
	var err error
	if j.Into != nil {
		po := j.partitionOptions()
		po.Progress = p
		st, err = imaging.RestorePartition(ctx, s.Repo, j.Snapshot, po)
		if st != nil && st.NewPartitionID != "" {
			res.Warnings = append(res.Warnings, "the partition got a new ID ("+st.NewPartitionID+"): its own is used by another partition in this computer")
		}
	} else {
		st, err = imaging.Restore(ctx, s.Repo, j.Snapshot, imaging.RestoreOptions{
			Image: j.Image, TargetDisk: j.TargetDisk, KeepOffline: j.KeepOffline, Partitions: j.Partitions, Layout: j.Layout, Progress: p,
		})
	}
	res.Restore = st
	if err != nil {
		log.Printf("image restore failed: %v", err)
		return res, err
	}
	log.Printf("wrote %d partitions, %d bytes in %s", st.Partitions, st.BytesWritten, st.Duration.Round(time.Second))
	logf := func(s string) { log.Printf("%s", s) }
	if !j.KeepOffline {
		res.Warnings = append(res.Warnings, imaging.FinishLayout(ctx, j.TargetDisk, st, logf)...)
	}
	needBoot := st.Moved
	if j.Into != nil {
		// Only a restored Windows partition needs new boot files.
		needBoot = imaging.HasWindows(ctx, s.Repo, imagePartition(img, j.Into.Partition))
	}
	if len(st.Shrunk) > 0 {
		res.Warnings = append(res.Warnings, "partitions were restored smaller: Windows checks their file systems (chkdsk) once at the next start")
	}
	if needBoot && !j.KeepOffline && !j.NewHardware {
		// Partitions moved: the BCD (on MBR disks) points at the old offsets.
		br, err := imaging.RebuildBoot(ctx, j.TargetDisk, logf)
		switch {
		case err != nil:
			res.Warnings = append(res.Warnings, "rebuild the boot files: "+err.Error())
		default:
			res.Boot = br
			res.Warnings = append(res.Warnings, br.Warnings...)
		}
	}
	if j.NewHardware && !j.KeepOffline {
		if progress != nil {
			progress("hardware", 0, 0)
		}
		dirs := append(imaging.RecoveryDriverDirs(), j.DriverDirs...)
		hw, err := imaging.PrepareForNewHardware(ctx, j.TargetDisk, dirs, func(s string) { log.Printf("new hardware: %s", s) })
		res.Hardware = hw
		if err != nil {
			res.Warnings = append(res.Warnings, "preparing for new hardware failed: "+err.Error())
		} else {
			res.Warnings = append(res.Warnings, hw.Warnings...)
		}
	}
	for _, w := range res.Warnings {
		log.Printf("warning: %s", w)
	}
	return res, nil
}
