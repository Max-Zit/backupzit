//go:build windows

package imaging

import (
	"context"
	"encoding/binary"
	"fmt"
	"strings"

	"golang.org/x/sys/windows"
)

const fsctlExtendVolume = 0x000900F0

// FinishLayout completes an image restore with a changed layout: it
// extends the file systems of partitions that became larger (the image
// holds them at their old size). It returns warnings; the restore itself
// has succeeded either way.
func FinishLayout(ctx context.Context, diskNumber int, st *RestoreStats, log func(string)) []string {
	if log == nil {
		log = func(string) {}
	}
	if st == nil || len(st.Grown) == 0 {
		return nil
	}
	var warnings []string
	disk, _, err := waitForWindows(ctx, diskNumber)
	if disk == nil {
		return []string{fmt.Sprintf("extend file systems: %v", err)}
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
