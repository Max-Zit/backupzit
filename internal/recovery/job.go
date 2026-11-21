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
	if j.Partitions != nil && len(j.Partitions) == 0 {
		return errors.New("no partition selected")
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
	sel := map[int]bool{}
	for _, n := range j.Partitions {
		sel[n] = true
	}
	var total, done uint64
	var parts []*repo.PartitionImage
	for i := range img.Partitions {
		p := &img.Partitions[i]
		if p.Included && (j.Partitions == nil || sel[p.Number]) {
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
	if j.Partitions != nil {
		log.Printf("partitions: %v", j.Partitions)
	}
	lock, err := s.Repo.Lock(ctx, false, time.Minute)
	if err != nil {
		return nil, fmt.Errorf("lock repository: %w", err)
	}
	defer func() { _ = lock.Unlock() }()

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
	st, err := imaging.Restore(ctx, s.Repo, j.Snapshot, imaging.RestoreOptions{
		Image: j.Image, TargetDisk: j.TargetDisk, KeepOffline: j.KeepOffline, Partitions: j.Partitions, Progress: p,
	})
	res.Restore = st
	if err != nil {
		log.Printf("image restore failed: %v", err)
		return res, err
	}
	log.Printf("wrote %d partitions, %d bytes in %s", st.Partitions, st.BytesWritten, st.Duration.Round(time.Second))
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
