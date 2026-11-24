package recovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
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
	// TargetID is the disk the restore was confirmed for. Run refuses a
	// disk with that number that is another one: a disk plugged in again
	// gets the next free number, and another disk can get the chosen one.
	// Nil checks the number only.
	TargetID *imaging.DiskIdentity
	// Protect lists paths whose disks must not be written: the repository,
	// the recovery medium, access packages. Run maps them to disks again
	// when it starts.
	Protect []string
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
// (the recovery medium, a local repository, access packages): they must
// not be restored to. A path is a local path (drive letter, \\?\ path,
// volume GUID path or a folder mount point) or a usb:// location, which is
// resolved to the disk that has its label now. Network locations hold
// nothing local and are skipped.
func ProtectedDisks(disks []imaging.Disk, paths []string) map[int]string {
	out := map[int]string{}
	for _, p := range paths {
		local := p
		if strings.HasPrefix(strings.ToLower(p), "usb://") {
			dir, _, err := resolveUSB(p)
			if err != nil {
				continue
			}
			local = dir
		}
		if n, ok := imaging.DiskOfPath(disks, local); ok {
			out[n] = p
		}
	}
	return out
}

// resolveUSB is backend.ResolveUSB; tests replace it.
var resolveUSB = backend.ResolveUSB

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
	if j.TargetID != nil {
		if err := j.TargetID.Matches(*target); err != nil {
			return err
		}
	}
	if target.System && j.Into == nil {
		// A partition restore may write into free space or over a data
		// partition of the running system's disk; the imaging checks keep
		// its system and Windows partitions.
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
		if err := j.Layout.Validate(img, imaging.ShapeLimits(img)); err != nil {
			return err
		}
		if j.KeepOffline && j.Layout.Style != img.Style {
			// Offline, nothing formats the new EFI partition or writes the
			// boot files, and the disk would not start. A converted disk
			// gets new identifiers, so it does not clash with the old one.
			return fmt.Errorf("a disk converted to %s cannot stay offline: its new EFI partition and boot files need it online. It gets new disk and partition IDs, so it can be online next to the old disk", strings.ToUpper(j.Layout.Style))
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
	// Incomplete is set when the data was written but a later step failed
	// or was skipped (file systems, boot files, new hardware): the disk may
	// not start yet. The command line exits with ExitIncomplete then.
	Incomplete bool
}

// ExitIncomplete is the exit code of a command line restore whose data was
// written while a finishing step failed (Result.Incomplete).
const ExitIncomplete = 3

// restoredWindows finds the Windows installation among the partitions the
// job restores and returns where it lies on the target disk.
func (j Job) restoredWindows(ctx context.Context, r *repo.Repository, img *repo.DiskImage, st *imaging.RestoreStats) (uint64, bool) {
	only := j.restoredPartitions()
	sel := map[int]bool{}
	for _, n := range only {
		sel[n] = true
	}
	for i := range img.Partitions {
		p := &img.Partitions[i]
		if !p.Included || only != nil && !sel[p.Number] || !imaging.HasWindows(ctx, r, p) {
			continue
		}
		switch {
		case j.Into != nil:
			return st.PartitionOffset, true
		case j.Layout != nil:
			for _, lp := range j.Layout.Partitions {
				if lp.Source == p.Number && lp.Restore {
					return lp.Offset, true
				}
			}
		default:
			return p.Offset, true
		}
	}
	return 0, false
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
	// The disk head carries the MBR boot code a same-style MBR restore
	// writes; it is checked like the partitions.
	if j.Into == nil && !img.Head.IsNull() {
		if _, err := r.LoadBlob(ctx, repo.DataBlob, img.Head); err != nil {
			return fmt.Errorf("disk head: %w", err)
		}
	}
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
	// The disks as they are now, not as they were when the job was made:
	// minutes may have passed, and disks may have been plugged in or out.
	disks, err := imaging.ListDisks()
	if err != nil {
		return nil, err
	}
	if err := j.Check(disks, ProtectedDisks(disks, j.Protect)); err != nil {
		log.Printf("refused: %v", err)
		return nil, err
	}
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
	if j.Into != nil {
		po := j.partitionOptions()
		po.Progress = p
		po.Identity = j.TargetID
		st, err = imaging.RestorePartition(ctx, s.Repo, j.Snapshot, po)
		if st != nil && st.NewPartitionID != "" {
			res.Warnings = append(res.Warnings, "the partition got a new ID ("+st.NewPartitionID+"): its own is used by another partition in this computer")
		}
	} else {
		st, err = imaging.Restore(ctx, s.Repo, j.Snapshot, imaging.RestoreOptions{
			Image: j.Image, TargetDisk: j.TargetDisk, KeepOffline: j.KeepOffline, Partitions: j.Partitions, Layout: j.Layout, Progress: p,
			Identity: j.TargetID,
		})
	}
	res.Restore = st
	if err != nil {
		log.Printf("image restore failed: %v", err)
		return res, err
	}
	log.Printf("wrote %d partitions, %d bytes in %s", st.Partitions, st.BytesWritten, st.Duration.Round(time.Second))
	logf := func(s string) { log.Printf("%s", s) }
	// Every later step works on the disk as just written: same device,
	// with the new table's identifiers.
	ref := imaging.DiskRef{Number: j.TargetDisk, ID: st.Table}
	winAt, hasWindows := j.restoredWindows(ctx, s.Repo, img, st)
	if !hasWindows {
		// A data disk needs no boot code (and waiting for its Windows
		// would only delay the result with a bogus warning).
		st.NeedMBRCode = false
	}
	if !j.KeepOffline {
		finish := imaging.FinishLayout(ctx, ref, st, logf)
		res.Warnings = append(res.Warnings, finish...)
		res.Incomplete = res.Incomplete || len(finish) > 0
	} else if st.Moved || len(st.Grown) > 0 || len(st.CreatedEFI) > 0 {
		res.Incomplete = true
		res.Warnings = append(res.Warnings, "the disk stayed offline, so larger partitions were not extended and the boot files were not rebuilt: remove the old disk, bring this one online and run the boot repair of the recovery program")
	}
	// New boot files are needed where the BCD no longer finds Windows: on
	// MBR, where it refers to partition offsets, and after a conversion
	// (a new table, a new EFI partition). On GPT with the same table the
	// BCD refers to the partition and disk GUIDs, which the restore keeps,
	// so moved or resized partitions start as they are; rewriting the boot
	// files there would only risk failing (bcdboot refuses the revoked boot
	// managers of old Windows versions on a Secure Boot machine).
	style := img.Style
	if j.Layout != nil {
		style = j.Layout.Style
	}
	needBoot := hasWindows && st.Moved && (style == imaging.StyleMBR || style != img.Style)
	if j.Into != nil {
		// Only a restored Windows partition needs new boot files, and only
		// on a disk that can start it: a GPT disk without an EFI system
		// partition holds it as data (an old C: restored to read files).
		needBoot = hasWindows
		if t, err := (imaging.DiskRef{Number: j.TargetDisk}).Pick(disks); needBoot && err == nil && t.Style == imaging.StyleGPT && !hasESP(t) {
			needBoot = false
			res.Warnings = append(res.Warnings, "the disk has no EFI system partition, so the restored Windows was not made startable from it; its files can be used as they are")
		}
	}
	if len(st.Shrunk) > 0 {
		res.Warnings = append(res.Warnings, "partitions were restored smaller: Windows checks their file systems (chkdsk) once at the next start")
		for _, sh := range st.Shrunk {
			if sh.ShadowCopiesRemoved {
				res.Warnings = append(res.Warnings, fmt.Sprintf("partition %d: its shadow copies (previous versions) were removed by the shrink", sh.Partition))
			}
		}
	}
	if needBoot && !j.KeepOffline && !j.NewHardware {
		// Partitions moved: the BCD (on MBR disks) points at the old
		// offsets. The boot files of the restored Windows are written,
		// not those of another Windows on the disk.
		br, err := imaging.RebuildBoot(ctx, ref, winAt, logf)
		switch {
		case err != nil:
			res.Incomplete = true
			res.Warnings = append(res.Warnings, "rebuild the boot files: "+err.Error())
		default:
			res.Boot = br
			res.Incomplete = res.Incomplete || !br.Rebuilt
			res.Warnings = append(res.Warnings, br.Warnings...)
		}
	}
	if j.NewHardware && !j.KeepOffline {
		if progress != nil {
			progress("hardware", 0, 0)
		}
		dirs := append(imaging.RecoveryDriverDirs(), j.DriverDirs...)
		hw, err := imaging.PrepareForNewHardware(ctx, ref, dirs, func(s string) { log.Printf("new hardware: %s", s) })
		res.Hardware = hw
		if err != nil {
			res.Incomplete = true
			res.Warnings = append(res.Warnings, "preparing for new hardware failed: "+err.Error())
		} else {
			res.Incomplete = res.Incomplete || !hw.BootRebuilt
			res.Warnings = append(res.Warnings, hw.Warnings...)
		}
	}
	for _, w := range res.Warnings {
		log.Printf("warning: %s", w)
	}
	return res, nil
}

// hasESP tells whether d has an EFI system partition.
func hasESP(d *imaging.Disk) bool {
	for _, p := range d.Partitions {
		if strings.EqualFold(p.GPTType, imaging.GPTTypeEFI) {
			return true
		}
	}
	return false
}
