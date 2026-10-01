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

	"github.com/backupzit/backupzit/internal/repo"
	"golang.org/x/sys/windows"
)

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
}

// RestoreStats summarises an image restore.
type RestoreStats struct {
	Partitions   int
	BytesWritten uint64
	Duration     time.Duration
}

// Restore writes a disk image to a physical disk: it recreates the
// partition table with the original disk and partition identifiers (so
// boot configuration keeps working) and writes every stored block.
func Restore(ctx context.Context, r *repo.Repository, sn *repo.Snapshot, opts RestoreOptions) (*RestoreStats, error) {
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
	if target.System {
		return nil, fmt.Errorf("disk %d holds the running Windows installation and cannot be overwritten", opts.TargetDisk)
	}
	var needed uint64
	for _, p := range img.Partitions {
		needed = max(needed, p.Offset+p.Length)
	}
	needed += 1 << 20 // room for the backup GPT
	if target.Size < needed {
		return nil, fmt.Errorf("target disk %d is too small: %d bytes, image needs %d", opts.TargetDisk, target.Size, needed)
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
			setDiskOffline(dh, false)
		}
		ioctl(dh, ioctlDiskUpdateProperties, nil, 0)
	}()

	if err := writeLayout(dh, img, target.Size); err != nil {
		return nil, fmt.Errorf("write partition table: %w", err)
	}
	f := os.NewFile(uintptr(dh), physicalDrivePath(target.Number))

	if img.Style == StyleMBR {
		if err := restoreMBRBootCode(ctx, r, f, img); err != nil {
			return nil, err
		}
	}

	var total, done uint64
	for _, p := range img.Partitions {
		if p.Included {
			total += p.StoredBytes
		}
	}
	st := &RestoreStats{}
	for i := range img.Partitions {
		p := &img.Partitions[i]
		if !p.Included {
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
			data, err := r.LoadBlob(ctx, repo.DataBlob, id)
			if err != nil {
				return st, fmt.Errorf("partition %d block %d: %w", p.Number, b, err)
			}
			if _, err := f.WriteAt(data, int64(p.Offset)+int64(b)*int64(p.BlockSize)); err != nil {
				return st, fmt.Errorf("partition %d block %d: write: %w", p.Number, b, err)
			}
			st.BytesWritten += uint64(len(data))
			done += uint64(len(data))
			if opts.Progress != nil {
				opts.Progress(done, total)
			}
		}
		st.Partitions++
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
		windows.CloseHandle(h)
		return 0, err
	}
	if _, err := ioctl(h, fsctlDismountVolume, nil, 0); err != nil {
		windows.CloseHandle(h)
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

// writeLayout creates a partition table identical to the image's.
func writeLayout(h windows.Handle, img *repo.DiskImage, diskSize uint64) error {
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

	parts := img.Partitions
	count := len(parts)
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
	for i, p := range parts {
		e := lay[48+i*144 : 48+(i+1)*144]
		if img.Style == StyleGPT {
			binary.LittleEndian.PutUint32(e[0:4], partitionStyleGPT)
		} else {
			binary.LittleEndian.PutUint32(e[0:4], partitionStyleMBR)
		}
		binary.LittleEndian.PutUint64(e[8:16], p.Offset)
		binary.LittleEndian.PutUint64(e[16:24], p.Length)
		binary.LittleEndian.PutUint32(e[24:28], uint32(p.Number))
		e[28] = 1 // RewritePartition
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
			u[2] = 1 // RecognizedPartition
			binary.LittleEndian.PutUint32(u[4:8], uint32(p.Offset/sector))
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
