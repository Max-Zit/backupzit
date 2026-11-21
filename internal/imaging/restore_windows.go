//go:build windows

package imaging

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf16"

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
	var target *Disk
	for i := range disks {
		if disks[i].Number == opts.TargetDisk {
			target = &disks[i]
		}
	}
	if target == nil {
		return nil, fmt.Errorf("target disk %d not found", opts.TargetDisk)
	}
	if target.System {
		return nil, fmt.Errorf("disk %d holds the running Windows installation and cannot be overwritten", opts.TargetDisk)
	}
	var last uint64
	for _, w := range plan {
		last = max(last, w.Offset+w.Length)
	}
	if _, end := UsableRange(img.Style, target.Size, target.SectorSize); end < last {
		return nil, fmt.Errorf("target disk %d is too small: %d bytes, the partitions need %d", opts.TargetDisk, target.Size, last+(target.Size-end))
	}
	if target.SectorSize != img.SectorSize {
		return nil, fmt.Errorf("target disk has %d byte sectors, image was taken from %d byte sectors", target.SectorSize, img.SectorSize)
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

	dh, err := openDevice(physicalDrivePath(target.Number), true)
	if err != nil {
		return nil, fmt.Errorf("open target disk: %w", err)
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

	if err := writeLayout(dh, img, plan, target.Size); err != nil {
		return nil, fmt.Errorf("write partition table: %w", err)
	}
	f := os.NewFile(uintptr(dh), physicalDrivePath(target.Number))

	if img.Style == StyleMBR {
		if err := restoreMBRBootCode(ctx, r, f, img); err != nil {
			return nil, err
		}
	}

	var total, done uint64
	for _, w := range plan {
		if w.Restore {
			total += w.Src.StoredBytes
		}
	}
	st := &RestoreStats{}
	for _, w := range plan {
		p := w.Src
		if !w.Restore {
			continue
		}
		ids, err := r.LoadBlockMap(ctx, p)
		if err != nil {
			return st, fmt.Errorf("partition %d: %w", p.Number, err)
		}
		for b, id := range ids {
			if err := ctx.Err(); err != nil {
				return st, err
			}
			if id.IsNull() {
				continue
			}
			off, ok := w.blockOffset(b)
			if !ok {
				continue
			}
			data, err := r.LoadBlob(ctx, repo.DataBlob, id)
			if err != nil {
				return st, fmt.Errorf("partition %d block %d: %w", p.Number, b, err)
			}
			if _, err := f.WriteAt(data, int64(off)); err != nil { // #nosec G115 -- disk offsets are within range
				return st, fmt.Errorf("partition %d block %d: write: %w", p.Number, b, err)
			}
			st.BytesWritten += uint64(len(data))
			done += uint64(len(data))
			if opts.Progress != nil {
				opts.Progress(done, total)
			}
		}
		st.Partitions++
		if w.Moved() {
			st.Moved = true
			if err := fixBootSectors(f, w, img.SectorSize); err != nil {
				return st, fmt.Errorf("partition %d: %w", p.Number, err)
			}
		}
		if w.Grown() {
			st.Grown = append(st.Grown, w.Offset)
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
		if rel+uint64(sectorSize) > w.Length {
			continue
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

// writeLayout creates the partition table: the image's disk and
// partition identifiers at the offsets and sizes of the plan.
func writeLayout(h windows.Handle, img *repo.DiskImage, plan []partitionWrite, diskSize uint64) error {
	// CREATE_DISK: PartitionStyle, then MBR {Signature} or GPT {DiskId, MaxPartitionCount}.
	create := make([]byte, 24)
	switch img.Style {
	case StyleGPT:
		binary.LittleEndian.PutUint32(create[0:4], partitionStyleGPT)
		id, err := guidBytes(img.GPTDiskID)
		if err != nil {
			return err
		}
		copy(create[4:20], id[:])
		binary.LittleEndian.PutUint32(create[20:24], 128)
	case StyleMBR:
		binary.LittleEndian.PutUint32(create[0:4], partitionStyleMBR)
		binary.LittleEndian.PutUint32(create[4:8], img.MBRSignature)
	default:
		return fmt.Errorf("unsupported partition style %q", img.Style)
	}
	if _, err := ioctl(h, ioctlDiskCreateDisk, create, 0); err != nil {
		return fmt.Errorf("create disk: %w", err)
	}

	count := len(plan)
	if img.Style == StyleMBR {
		count = (count + 3) / 4 * 4 // MBR layouts come in groups of four
		if count == 0 {
			count = 4
		}
	}
	lay := make([]byte, 48+144*count)
	sector := uint64(img.SectorSize)
	binary.LittleEndian.PutUint32(lay[4:8], uint32(count))
	if img.Style == StyleGPT {
		binary.LittleEndian.PutUint32(lay[0:4], partitionStyleGPT)
		id, _ := guidBytes(img.GPTDiskID)
		copy(lay[8:24], id[:])
		first := 34 * sector
		binary.LittleEndian.PutUint64(lay[24:32], first)
		binary.LittleEndian.PutUint64(lay[32:40], diskSize-first-33*sector)
		binary.LittleEndian.PutUint32(lay[40:44], 128)
	} else {
		binary.LittleEndian.PutUint32(lay[0:4], partitionStyleMBR)
		binary.LittleEndian.PutUint32(lay[8:12], img.MBRSignature)
	}
	for i, w := range plan {
		p := w.Src
		e := lay[48+i*144 : 48+(i+1)*144]
		if img.Style == StyleGPT {
			binary.LittleEndian.PutUint32(e[0:4], partitionStyleGPT)
		} else {
			binary.LittleEndian.PutUint32(e[0:4], partitionStyleMBR)
		}
		binary.LittleEndian.PutUint64(e[8:16], w.Offset)
		binary.LittleEndian.PutUint64(e[16:24], w.Length)
		binary.LittleEndian.PutUint32(e[24:28], uint32(p.Number)) // #nosec G115 -- length of a small buffer for a Windows API call
		e[28] = 1                                                 // RewritePartition
		u := e[32:]
		if img.Style == StyleGPT {
			t, err := guidBytes(p.GPTType)
			if err != nil {
				return fmt.Errorf("partition %d type: %w", p.Number, err)
			}
			id, err := guidBytes(p.GPTID)
			if err != nil {
				return fmt.Errorf("partition %d id: %w", p.Number, err)
			}
			copy(u[0:16], t[:])
			copy(u[16:32], id[:])
			binary.LittleEndian.PutUint64(u[32:40], p.GPTAttributes)
			name := utf16.Encode([]rune(p.Name))
			for j := 0; j < len(name) && j < 35; j++ {
				binary.LittleEndian.PutUint16(u[40+2*j:], name[j])
			}
		} else {
			u[0] = p.MBRType
			if p.Bootable {
				u[1] = 1
			}
			u[2] = 1                                                       // RecognizedPartition
			binary.LittleEndian.PutUint32(u[4:8], uint32(w.Offset/sector)) // #nosec G115 -- length of a small buffer for a Windows API call
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
func restoreMBRBootCode(ctx context.Context, r *repo.Repository, f *os.File, img *repo.DiskImage) error {
	head, err := r.LoadBlob(ctx, repo.DataBlob, img.Head)
	if err != nil {
		return fmt.Errorf("disk head: %w", err)
	}
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
