package imaging

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/max-zit/backupzit/internal/repo"
)

// ErrShrinkUnsupported is returned for a layout that makes a partition
// smaller than in the image that is not NTFS (only NTFS can be shrunk).
var ErrShrinkUnsupported = errors.New("only NTFS partitions can be restored smaller than they were backed up")

// partitionWrite is one partition of the target disk: where it goes and
// whether its contents come from the image.
type partitionWrite struct {
	Src     *repo.PartitionImage
	Offset  uint64
	Length  uint64
	Restore bool
	// Shrink is set when an NTFS partition becomes smaller: its file
	// system is shrunk while it is written.
	Shrink bool
}

// Moved tells whether the partition changes place or size.
func (w partitionWrite) Moved() bool { return w.Offset != w.Src.Offset || w.Length != w.Src.Length }

// Grown tells whether the partition becomes larger.
func (w partitionWrite) Grown() bool { return w.Length > w.Src.Length }

// restorePlan works out the partitions of the target disk: the image as
// it is, or the layout of opts.
func restorePlan(img *repo.DiskImage, opts RestoreOptions) ([]partitionWrite, error) {
	sel := func(p *repo.PartitionImage) bool {
		if !p.Included {
			return false
		}
		if opts.Partitions == nil {
			return true
		}
		for _, n := range opts.Partitions {
			if n == p.Number {
				return true
			}
		}
		return false
	}
	for _, n := range opts.Partitions {
		p := findPartition(img, n)
		if p == nil {
			return nil, fmt.Errorf("the image has no partition %d", n)
		}
		if !p.Included {
			return nil, fmt.Errorf("partition %d was not backed up", n)
		}
	}
	var out []partitionWrite
	if opts.Layout == nil {
		for i := range img.Partitions {
			p := &img.Partitions[i]
			out = append(out, partitionWrite{Src: p, Offset: p.Offset, Length: p.Length, Restore: sel(p)})
		}
		return out, nil
	}
	if opts.Partitions != nil {
		return nil, errors.New("give the partitions to restore either in the layout or as a list, not both")
	}
	l := opts.Layout
	if l.Style != img.Style {
		return nil, fmt.Errorf("converting %s to %s is not supported yet", strings.ToUpper(img.Style), strings.ToUpper(l.Style))
	}
	for _, lp := range l.Partitions {
		p := findPartition(img, lp.Source)
		if p == nil {
			return nil, fmt.Errorf("the image has no partition %d", lp.Source)
		}
		if lp.Restore && !p.Included {
			return nil, fmt.Errorf("partition %d was not backed up", lp.Source)
		}
		w := partitionWrite{Src: p, Offset: lp.Offset, Length: lp.Length, Restore: lp.Restore}
		if lp.Restore && lp.Length < p.Length {
			if !strings.EqualFold(p.FileSystem, "NTFS") || FixedSizeKind(p) {
				return nil, fmt.Errorf("partition %d: %w", lp.Source, ErrShrinkUnsupported)
			}
			w.Shrink = true
		}
		out = append(out, w)
	}
	return out, nil
}

// blockOffset returns where block b of a partition goes on the target
// disk, and false for blocks past the end of the target partition.
func (w partitionWrite) blockOffset(b int) (uint64, bool) {
	rel := uint64(b) * uint64(w.Src.BlockSize) // #nosec G115 -- block index is never negative
	if rel >= w.Length {
		return 0, false
	}
	return w.Offset + rel, true
}

// Boot sector fields shared by NTFS and FAT.
const bootHiddenSectors = 0x1C // uint32: sectors before the partition (MBR)

// bootSectorKind returns "ntfs", "fat32", "fat" or "" for a boot sector.
func bootSectorKind(b []byte) string {
	if len(b) < 512 || b[510] != 0x55 || b[511] != 0xAA {
		return ""
	}
	switch {
	case string(b[3:11]) == "NTFS    ":
		return "ntfs"
	case string(b[0x52:0x5A]) == "FAT32   ":
		return "fat32"
	case string(b[0x36:0x3E]) == "FAT16   " || string(b[0x36:0x3E]) == "FAT12   " || string(b[0x36:0x3B]) == "FAT  ":
		return "fat"
	}
	return ""
}

// patchHiddenSectors sets the number of sectors before the partition in
// an NTFS or FAT boot sector; on MBR disks Windows' boot code uses it to
// find the partition. It reports whether the sector was changed.
func patchHiddenSectors(b []byte, offset uint64, sectorSize uint32) bool {
	if bootSectorKind(b) == "" {
		return false
	}
	hidden := offset / uint64(sectorSize)
	if hidden > 0xFFFFFFFF {
		hidden = 0 // beyond 2 TiB the field cannot hold it; GPT ignores it
	}
	if binary.LittleEndian.Uint32(b[bootHiddenSectors:]) == uint32(hidden) {
		return false
	}
	binary.LittleEndian.PutUint32(b[bootHiddenSectors:], uint32(hidden))
	return true
}

// backupBootSectors returns the offsets (relative to the partition) of
// the backup copies of a boot sector: NTFS keeps one in the last sector
// of the volume, FAT32 in sector 6.
func backupBootSectors(b []byte, sectorSize uint32) []uint64 {
	switch bootSectorKind(b) {
	case "ntfs":
		g, err := ParseNTFSBoot(b)
		if err != nil {
			return nil
		}
		return []uint64{g.TotalSectors * uint64(g.BytesPerSector)}
	case "fat32":
		bs := uint64(binary.LittleEndian.Uint16(b[0x32:]))
		if bs == 0 || bs == 0xFFFF {
			return nil
		}
		return []uint64{bs * uint64(sectorSize)}
	}
	return nil
}
