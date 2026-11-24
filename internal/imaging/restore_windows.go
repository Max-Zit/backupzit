//go:build windows

package imaging

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/max-zit/backupzit/internal/imaging/ntfsresize"
	"github.com/max-zit/backupzit/internal/repo"
	"golang.org/x/sys/windows"
)

// Restore writes a disk image to a physical disk: it recreates the
// partition table with the original disk and partition identifiers (so
// boot configuration keeps working) and writes every stored block.
func Restore(ctx context.Context, r *repo.Repository, sn *repo.Snapshot, opts RestoreOptions) (*RestoreStats, error) {
	if opts.Image < 0 || opts.Image >= len(sn.Images) {
		return nil, fmt.Errorf("snapshot %s has no disk image %d", sn.ID.Short(), opts.Image)
	}
	img := &sn.Images[opts.Image]
	start := time.Now()
	plan, err := restorePlan(img, opts)
	if err != nil {
		return nil, err
	}

	disks, err := ListDisks()
	if err != nil {
		return nil, err
	}
	target, err := DiskRef{Number: opts.TargetDisk, ID: opts.Identity}.Pick(disks)
	if err != nil {
		return nil, fmt.Errorf("target: %w", err)
	}
	if target.System {
		return nil, fmt.Errorf("disk %d holds the running Windows installation and cannot be overwritten", opts.TargetDisk)
	}
	var last uint64
	for _, w := range plan {
		last = max(last, w.Offset+w.Length)
	}
	style := img.Style
	if opts.Layout != nil {
		style = opts.Layout.Style
	}
	if _, end := UsableRange(style, target.Size, target.SectorSize); end < last {
		return nil, fmt.Errorf("target disk %d is too small: %d bytes, the partitions need %d", opts.TargetDisk, target.Size, last+(target.Size-end))
	}
	if target.SectorSize != img.SectorSize {
		return nil, fmt.Errorf("target disk has %d byte sectors, image was taken from %d byte sectors", target.SectorSize, img.SectorSize)
	}

	// Plan the shrinking of NTFS partitions before the disk is touched, so
	// a volume that cannot be shrunk leaves the target as it was.
	shrinks := map[int]*ntfsresize.Plan{}
	for i, w := range plan {
		if w.Shrink {
			sp, err := planShrink(ctx, r, w)
			if err != nil {
				return nil, err
			}
			shrinks[i] = sp
		}
	}

	diskID, sig := img.GPTDiskID, img.MBRSignature
	if style != img.Style {
		// A converted disk gets new identifiers; its boot files are written
		// afterwards anyway.
		if diskID, err = NewGUID(); err != nil {
			return nil, err
		}
		sig = newMBRSignature()
	}
	if style == StyleGPT {
		if _, err := guidBytesPortable(diskID); err != nil {
			return nil, fmt.Errorf("GPT disk ID of the image: %w", err)
		}
	}

	// Load everything small the restore needs before the disk is touched:
	// the disk head (MBR boot code) and the block map of every restored
	// partition. A damaged one then leaves the target as it was.
	var head []byte
	if style == StyleMBR && img.Style == StyleMBR {
		if head, err = r.LoadBlob(ctx, repo.DataBlob, img.Head); err != nil {
			return nil, fmt.Errorf("disk head: %w", err)
		}
		if len(head) < 440 {
			return nil, fmt.Errorf("disk head: %d bytes, too short for MBR boot code", len(head))
		}
	}
	maps := map[int][]repo.ID{}
	for i, w := range plan {
		if !w.Restore {
			continue
		}
		ids, err := r.LoadBlockMap(ctx, w.Src)
		if err != nil {
			return nil, fmt.Errorf("partition %d: %w", w.Src.Number, err)
		}
		maps[i] = ids
	}

	// Try to lock and dismount volumes on the target disk so pending writes
	// are flushed. Antivirus or indexing often keeps them open; taking the
	// disk offline below force-dismounts them anyway.
	for _, p := range target.Partitions {
		if p.VolumeGUIDPath == "" {
			continue
		}
		if h, err := lockVolume(p.VolumeGUIDPath); err == nil {
			defer windows.CloseHandle(h)
		}
	}

	dh, err := openTargetChecked(target.Number, opts.Identity)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(dh)

	// Take the disk offline while writing so Windows does not mount the
	// new, still empty partitions (raw writes to mounted volumes fail).
	if err := setDiskOffline(dh, true); err != nil {
		return nil, fmt.Errorf("take target disk offline: %w", err)
	}
	defer func() {
		if !opts.KeepOffline {
			_ = setDiskOffline(dh, false)
		}
		_, _ = ioctl(dh, ioctlDiskUpdateProperties, nil, 0)
	}()

	if err := writeLayout(dh, style, diskID, sig, plan, img.SectorSize, target.Size); err != nil {
		return nil, fmt.Errorf("write partition table: %w", err)
	}
	f := os.NewFile(uintptr(dh), physicalDrivePath(target.Number))

	if style == StyleMBR && img.Style == StyleMBR {
		if err := restoreMBRBootCode(f, img, head); err != nil {
			return nil, err
		}
	}
	// A partition created empty where the target had none would show the
	// old contents of its sectors: a stale file system could mount, and a
	// new EFI partition would not count as unformatted. Its first and last
	// MiB are cleared. One in exactly the place of an old partition keeps
	// that partition's data, as before (a restore of only some partitions
	// onto the original disk).
	for _, w := range plan {
		if w.Restore || samePlace(target, w) {
			continue
		}
		if err := clearEnds(&sectionWriter{f: f, base: w.Offset, size: w.Length}, w.Length); err != nil {
			return nil, fmt.Errorf("clear the new partition at %d: %w", w.Offset, err)
		}
	}

	var total, done uint64
	for _, w := range plan {
		if w.Restore {
			total += w.Src.StoredBytes
		}
	}
	st := &RestoreStats{NeedMBRCode: style == StyleMBR && img.Style != StyleMBR}
	table := target.Identity().WithTable(style, diskID, sig)
	st.Table = &table
	for _, w := range plan {
		if w.Create == SourceNewEFI {
			st.CreatedEFI = append(st.CreatedEFI, w.Offset)
		}
	}
	for i, w := range plan {
		if !w.Restore {
			continue
		}
		err := writePartition(ctx, r, f, w, maps[i], shrinks[i], img.SectorSize, st, func(n uint64) {
			done += n
			if opts.Progress != nil {
				opts.Progress(done, total)
			}
		})
		if err != nil {
			return st, err
		}
	}
	for _, w := range plan {
		if !w.Restore && w.Moved() {
			st.Moved = true
		}
	}
	if err := windows.FlushFileBuffers(dh); err != nil {
		return st, fmt.Errorf("flush: %w", err)
	}
	st.Duration = time.Since(start)
	return st, nil
}

func lockVolume(guidPath string) (windows.Handle, error) {
	h, err := openDevice(strings.TrimSuffix(guidPath, `\`), true)
	if err != nil {
		return 0, err
	}
	if _, err := ioctl(h, fsctlLockVolume, nil, 0); err != nil {
		_ = windows.CloseHandle(h)
		return 0, err
	}
	if _, err := ioctl(h, fsctlDismountVolume, nil, 0); err != nil {
		_ = windows.CloseHandle(h)
		return 0, err
	}
	return h, nil
}

func setDiskOffline(h windows.Handle, offline bool) error {
	// SET_DISK_ATTRIBUTES: Version, Persist, Reserved1[3], Attributes, AttributesMask, Reserved2[4]
	in := make([]byte, 40)
	binary.LittleEndian.PutUint32(in[0:4], 40)
	var attrs uint64
	if offline {
		attrs = diskAttributeOffline
	}
	binary.LittleEndian.PutUint64(in[8:16], attrs)
	binary.LittleEndian.PutUint64(in[16:24], diskAttributeOffline)
	_, err := ioctl(h, ioctlDiskSetDiskAttributes, in, 0)
	return err
}

// planShrink plans shrinking the NTFS file system of w before anything is
// written.
func planShrink(ctx context.Context, r *repo.Repository, w partitionWrite) (*ntfsresize.Plan, error) {
	pr, err := NewPartitionReader(ctx, r, w.Src)
	if err != nil {
		return nil, fmt.Errorf("partition %d: %w", w.Src.Number, err)
	}
	vol, err := ntfsresize.Open(pr)
	if err != nil {
		return nil, fmt.Errorf("partition %d: read the file system: %w", w.Src.Number, err)
	}
	// A VSS image may have an open $LogFile (taken while Windows ran);
	// any other image is shrunk only without moving data.
	sp, err := vol.PlanWith(w.Length, ShrinkOptions(w.Src))
	if err != nil {
		return nil, fmt.Errorf("partition %d cannot be shrunk to %d bytes: %w", w.Src.Number, w.Length, err)
	}
	return sp, nil
}

// writePartition writes the stored blocks of one partition to its place
// on the disk, shrinks its file system (sp, if set) and fixes the boot
// sector of a moved partition. written is called with each block's size.
func writePartition(ctx context.Context, r *repo.Repository, f *os.File, w partitionWrite, ids []repo.ID, sp *ntfsresize.Plan, sectorSize uint32, st *RestoreStats, written func(uint64)) error {
	p := w.Src
	if ids == nil {
		var err error
		if ids, err = r.LoadBlockMap(ctx, p); err != nil {
			return fmt.Errorf("partition %d: %w", p.Number, err)
		}
	}
	// Every block goes through the partition's bounds: image metadata
	// cannot make a write leave [Offset, Offset+Length).
	part := &sectionWriter{f: f, base: w.Offset, size: w.Length}
	// The next blocks load while one is written (issue: ~45 MB/s from SMB
	// on a 1 GBit/s link with one block at a time).
	ra := r.ReadAhead(ctx, ids, func(b int) bool {
		_, ok := w.blockRel(b)
		return ok && !ids[b].IsNull()
	})
	defer ra.Close()
	for b, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		if id.IsNull() {
			continue
		}
		rel, ok := w.blockRel(b)
		if !ok {
			continue
		}
		data, err := ra.Get(b)
		if err != nil {
			return fmt.Errorf("partition %d block %d: %w", p.Number, b, err)
		}
		if uint64(len(data)) > w.Length-rel {
			data = data[:w.Length-rel] // a shrunk partition ends inside the block
		}
		if _, err := part.WriteAt(data, int64(rel)); err != nil { // #nosec G115 -- rel < Length, which fits the disk
			return fmt.Errorf("partition %d block %d: write: %w", p.Number, b, err)
		}
		st.BytesWritten += uint64(len(data))
		written(uint64(len(data)))
	}
	if sp != nil {
		if err := sp.Apply(&sectionWriter{f: f, base: w.Offset, size: w.Length}, nil); err != nil {
			return fmt.Errorf("partition %d: shrink the file system: %w; the target is incomplete, run the restore again", p.Number, err)
		}
		// Read back what was written before Windows tools (bcdboot, DISM)
		// mount it read-write.
		if err := sp.Verify(ntfsresize.NewAlignedReader(f, int64(w.Offset), int64(w.Length), int(sectorSize))); err != nil { // #nosec G115 -- disk offsets and sizes fit int64
			return fmt.Errorf("partition %d: the shrunk file system does not check out: %w; the target is incomplete, run the restore again", p.Number, err)
		}
		st.Shrunk = append(st.Shrunk, ShrinkReport{Partition: p.Number, MovedClusters: sp.MovedClusters, Orphaned: sp.Orphaned, ShadowCopiesRemoved: sp.ShadowCopiesRemoved})
	}
	st.Partitions++
	if w.Moved() {
		st.Moved = true
	}
	if w.Moved() || w.Converted {
		if err := fixBootSectors(f, w, sectorSize); err != nil {
			return fmt.Errorf("partition %d: %w", p.Number, err)
		}
	}
	if w.Grown() {
		st.Grown = append(st.Grown, w.Offset)
	}
	return nil
}

// sectionWriter writes into one partition of the disk.
type sectionWriter struct {
	f          *os.File
	base, size uint64
}

func (s *sectionWriter) WriteAt(b []byte, off int64) (int, error) {
	if off < 0 || uint64(off) > s.size || uint64(len(b)) > s.size-uint64(off) {
		return 0, fmt.Errorf("write at %d+%d outside the partition (%d bytes)", off, len(b), s.size)
	}
	return s.f.WriteAt(b, int64(s.base)+off) // #nosec G115 -- disk offsets are within range
}

// fixBootSectors updates the boot sector of a moved partition (and its
// backup copies) with the new number of sectors before the partition.
func fixBootSectors(f *os.File, w partitionWrite, sectorSize uint32) error {
	sec := make([]byte, sectorSize)
	if _, err := f.ReadAt(sec, int64(w.Offset)); err != nil { // #nosec G115 -- disk offsets are within range
		return fmt.Errorf("read boot sector: %w", err)
	}
	backups := backupBootSectors(sec, sectorSize)
	if !patchHiddenSectors(sec, w.Offset, sectorSize) {
		return nil
	}
	if _, err := f.WriteAt(sec, int64(w.Offset)); err != nil { // #nosec G115 -- disk offsets are within range
		return fmt.Errorf("write boot sector: %w", err)
	}
	for _, rel := range backups {
		if w.Length < uint64(sectorSize) || rel > w.Length-uint64(sectorSize) {
			continue // outside the partition (damaged or crafted boot sector)
		}
		b := make([]byte, sectorSize)
		if _, err := f.ReadAt(b, int64(w.Offset+rel)); err != nil { // #nosec G115 -- disk offsets are within range
			continue
		}
		if patchHiddenSectors(b, w.Offset, sectorSize) {
			if _, err := f.WriteAt(b, int64(w.Offset+rel)); err != nil { // #nosec G115 -- disk offsets are within range
				return fmt.Errorf("write backup boot sector: %w", err)
			}
		}
	}
	return nil
}

// newMBRSignature returns a random, non-zero MBR disk signature.
func newMBRSignature() uint32 {
	for {
		var b [4]byte
		_, _ = rand.Read(b[:])
		if s := binary.LittleEndian.Uint32(b[:]); s != 0 {
			return s
		}
	}
}

// writeLayout creates the partition table in the given style and disk
// identity, with the entries of the plan.
func writeLayout(h windows.Handle, style, diskID string, signature uint32, plan []partitionWrite, sectorSize uint32, diskSize uint64) error {
	// CREATE_DISK: PartitionStyle, then MBR {Signature} or GPT {DiskId, MaxPartitionCount}.
	create := make([]byte, 24)
	switch style {
	case StyleGPT:
		binary.LittleEndian.PutUint32(create[0:4], partitionStyleGPT)
		id, err := guidBytes(diskID)
		if err != nil {
			return err
		}
		copy(create[4:20], id[:])
		binary.LittleEndian.PutUint32(create[20:24], 128)
	case StyleMBR:
		binary.LittleEndian.PutUint32(create[0:4], partitionStyleMBR)
		binary.LittleEndian.PutUint32(create[4:8], signature)
	default:
		return fmt.Errorf("unsupported partition style %q", style)
	}
	if _, err := ioctl(h, ioctlDiskCreateDisk, create, 0); err != nil {
		return fmt.Errorf("create disk: %w", err)
	}

	count := len(plan)
	if style == StyleMBR {
		count = (count + 3) / 4 * 4 // MBR layouts come in groups of four
		if count == 0 {
			count = 4
		}
	}
	lay := make([]byte, 48+144*count)
	sector := uint64(sectorSize)
	binary.LittleEndian.PutUint32(lay[4:8], uint32(count)) // #nosec G115 -- small count
	if style == StyleGPT {
		binary.LittleEndian.PutUint32(lay[0:4], partitionStyleGPT)
		id, _ := guidBytes(diskID)
		copy(lay[8:24], id[:])
		first, end := UsableRange(StyleGPT, diskSize, sectorSize)
		binary.LittleEndian.PutUint64(lay[24:32], first)
		binary.LittleEndian.PutUint64(lay[32:40], end-first)
		binary.LittleEndian.PutUint32(lay[40:44], 128)
	} else {
		binary.LittleEndian.PutUint32(lay[0:4], partitionStyleMBR)
		binary.LittleEndian.PutUint32(lay[8:12], signature)
	}
	for i, w := range plan {
		e := lay[48+i*144 : 48+(i+1)*144]
		num := w.Src.Number
		if num <= 0 {
			num = i + 1
		}
		if style == StyleGPT {
			binary.LittleEndian.PutUint32(e[0:4], partitionStyleGPT)
		} else {
			binary.LittleEndian.PutUint32(e[0:4], partitionStyleMBR)
		}
		binary.LittleEndian.PutUint64(e[8:16], w.Offset)
		binary.LittleEndian.PutUint64(e[16:24], w.Length)
		binary.LittleEndian.PutUint32(e[24:28], uint32(num)) // #nosec G115 -- small partition number
		e[28] = 1                                            // RewritePartition
		u := e[32:]
		if style == StyleGPT {
			t, err := guidBytes(w.GPTType)
			if err != nil {
				return fmt.Errorf("partition %d type: %w", num, err)
			}
			id, err := guidBytes(w.GPTID)
			if err != nil {
				return fmt.Errorf("partition %d id: %w", num, err)
			}
			copy(u[0:16], t[:])
			copy(u[16:32], id[:])
			binary.LittleEndian.PutUint64(u[32:40], w.GPTAttributes)
			name := utf16.Encode([]rune(w.Name))
			for j := 0; j < len(name) && j < 35; j++ {
				binary.LittleEndian.PutUint16(u[40+2*j:], name[j])
			}
		} else {
			u[0] = w.MBRType
			if w.Bootable {
				u[1] = 1
			}
			u[2] = 1                                                       // RecognizedPartition
			binary.LittleEndian.PutUint32(u[4:8], uint32(w.Offset/sector)) // #nosec G115 -- MBR offsets are below 2^32 sectors
		}
	}
	if _, err := ioctl(h, ioctlDiskSetDriveLayoutEx, lay, 0); err != nil {
		return fmt.Errorf("set layout: %w", err)
	}
	if _, err := ioctl(h, ioctlDiskUpdateProperties, nil, 0); err != nil && !errors.Is(err, windows.ERROR_NOT_READY) {
		return fmt.Errorf("update properties: %w", err)
	}
	return nil
}

// restoreMBRBootCode writes the original boot code (first 440 bytes of
// sector 0) while keeping the partition table Windows just wrote.
func restoreMBRBootCode(f *os.File, img *repo.DiskImage, head []byte) error {
	sec := make([]byte, img.SectorSize)
	if _, err := f.ReadAt(sec, 0); err != nil {
		return fmt.Errorf("read sector 0: %w", err)
	}
	copy(sec[:440], head[:440])
	if _, err := f.WriteAt(sec, 0); err != nil {
		return fmt.Errorf("write boot code: %w", err)
	}
	return nil
}
