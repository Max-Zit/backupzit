//go:build windows

package imaging

import (
	"context"
	"encoding/binary"
	"fmt"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

const fsctlExtendVolume = 0x000900F0

// FinishLayout completes an image restore with a changed layout: it
// formats EFI system partitions the restore created, extends the file
// systems of partitions that became larger (the image holds them at their
// old size) and writes the MBR boot code after a GPT to MBR conversion.
// It returns warnings; the restore itself has succeeded either way.
func FinishLayout(ctx context.Context, diskNumber int, st *RestoreStats, log func(string)) []string {
	if log == nil {
		log = func(string) {}
	}
	if st == nil || (len(st.Grown) == 0 && len(st.CreatedEFI) == 0 && !st.NeedMBRCode) {
		return nil
	}
	var warnings []string
	disk, winPart, err := waitForWindows(ctx, diskNumber)
	if disk == nil {
		return []string{fmt.Sprintf("finish the partitions: %v", err)}
	}
	for _, off := range st.CreatedEFI {
		if err := formatEFI(ctx, diskNumber, off); err != nil {
			warnings = append(warnings, fmt.Sprintf("format the new EFI system partition: %v", err))
			continue
		}
		log("EFI system partition formatted (FAT32)")
	}
	if st.NeedMBRCode {
		if winPart == nil {
			warnings = append(warnings, "no Windows partition found; the MBR boot code was not written")
		} else if win, unmount, err := letterFor(winPart.VolumeGUIDPath); err != nil {
			warnings = append(warnings, "write the MBR boot code: "+err.Error())
		} else {
			if err := writeMBRCode(ctx, win); err != nil {
				warnings = append(warnings, "write the MBR boot code: "+err.Error())
			} else {
				log("MBR boot code written")
			}
			unmount()
		}
	}
	for _, off := range st.Grown {
		var part *Partition
		for i := range disk.Partitions {
			if disk.Partitions[i].Offset == off {
				part = &disk.Partitions[i]
			}
		}
		if part == nil || part.VolumeGUIDPath == "" {
			warnings = append(warnings, fmt.Sprintf("partition at %d: no volume to extend; extend it in Disk Management", off))
			continue
		}
		if !strings.EqualFold(part.FileSystem, "NTFS") {
			warnings = append(warnings, fmt.Sprintf("partition %d (%s): only NTFS is extended automatically", part.Number, part.FileSystem))
			continue
		}
		if err := extendVolume(part.VolumeGUIDPath, part.Length/uint64(disk.SectorSize)); err != nil {
			warnings = append(warnings, fmt.Sprintf("partition %d: extend the file system: %v; extend it in Disk Management", part.Number, err))
			continue
		}
		log(fmt.Sprintf("partition %d: file system extended to %d bytes", part.Number, part.Length))
	}
	return warnings
}

// extendVolume grows a mounted NTFS volume to sectors sectors (the size
// of its partition), as "diskpart extend filesystem" does.
func extendVolume(guidPath string, sectors uint64) error {
	h, err := openDevice(strings.TrimSuffix(guidPath, `\`), true)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	in := make([]byte, 8)
	binary.LittleEndian.PutUint64(in, sectors)
	_, err = ioctl(h, fsctlExtendVolume, in, 0)
	return err
}

// formatEFI formats the partition at offset off of a disk with FAT32
// (an EFI system partition the restore created).
func formatEFI(ctx context.Context, diskNumber int, off uint64) error {
	// The new partition shows up as a volume once Windows has seen the
	// partition table; it has no file system yet.
	var part *Partition
	for i := 0; i < 15 && part == nil; i++ {
		disks, err := ListDisks()
		if err != nil {
			return err
		}
		for _, d := range disks {
			if d.Number != diskNumber {
				continue
			}
			for j := range d.Partitions {
				if d.Partitions[j].Offset == off && d.Partitions[j].VolumeGUIDPath != "" {
					part = &d.Partitions[j]
				}
			}
		}
		if part == nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
	}
	if part == nil {
		return fmt.Errorf("no volume at offset %d", off)
	}
	letter, unmount, err := letterFor(part.VolumeGUIDPath)
	if err != nil {
		return err
	}
	defer unmount()
	out, err := run(ctx, "format.com", letter, "/FS:FAT32", "/Q", "/Y", "/V:SYSTEM")
	if err != nil {
		return fmt.Errorf("format %s: %v: %s", letter, err, strings.TrimSpace(out))
	}
	return nil
}
