package imaging

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf16"

	"github.com/max-zit/backupzit/internal/repo"
)

// PartitionRestoreOptions configure a partition restore: one partition of
// an image is written to a disk whose other partitions stay as they are.
type PartitionRestoreOptions struct {
	// Image selects the disk image within the snapshot.
	Image int
	// Partition is the number of the partition in the image.
	Partition  int
	TargetDisk int
	// Replace is the number of the partition on the target disk that is
	// overwritten; 0 writes into free space.
	Replace int
	// Offset places the partition in free space (0: the first free space
	// that fits). Ignored with Replace.
	Offset uint64
	// Length of the new partition; 0 keeps the size of the replaced
	// partition, or the image partition's size in free space.
	Length   uint64
	Progress func(done, total uint64)
}

// freeRanges lists the unpartitioned ranges of a disk (aligned to
// LayoutAlign), optionally treating one partition as free.
func freeRanges(d *Disk, ignore int) [][2]uint64 {
	first, end := UsableRange(d.Style, d.Size, d.SectorSize)
	parts := make([]Partition, 0, len(d.Partitions))
	for _, p := range d.Partitions {
		if p.Number != ignore && p.Length > 0 {
			parts = append(parts, p)
		}
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].Offset < parts[j].Offset })
	var out [][2]uint64
	pos := alignUp(first, LayoutAlign)
	for _, p := range parts {
		if p.Offset > pos {
			out = append(out, [2]uint64{pos, alignDown(p.Offset, LayoutAlign)})
		}
		pos = max(pos, alignUp(p.End(), LayoutAlign))
	}
	if e := alignDown(end, LayoutAlign); e > pos {
		out = append(out, [2]uint64{pos, e})
	}
	return out
}

// planPartitionRestore works out where the image partition goes on the
// target disk.
func planPartitionRestore(img *repo.DiskImage, target *Disk, opts PartitionRestoreOptions) (partitionWrite, error) {
	src := findPartition(img, opts.Partition)
	if src == nil {
		return partitionWrite{}, fmt.Errorf("the image has no partition %d", opts.Partition)
	}
	if !src.Included {
		return partitionWrite{}, fmt.Errorf("partition %d was not backed up", opts.Partition)
	}
	if target.Style != StyleGPT && target.Style != StyleMBR {
		return partitionWrite{}, errors.New("the target disk has no partition table; restore the whole disk instead")
	}
	if target.SectorSize != img.SectorSize {
		return partitionWrite{}, fmt.Errorf("target disk has %d byte sectors, image was taken from %d byte sectors", target.SectorSize, img.SectorSize)
	}
	if target.Style != img.Style && FixedSizeKind(src) {
		return partitionWrite{}, fmt.Errorf("a %s partition cannot be put on a %s disk", Partition{GPTType: src.GPTType, MBRType: src.MBRType}.Kind(), strings.ToUpper(target.Style))
	}
	w := partitionWrite{Src: src, Restore: true}
	if opts.Replace > 0 {
		var old *Partition
		for i := range target.Partitions {
			if target.Partitions[i].Number == opts.Replace {
				old = &target.Partitions[i]
			}
		}
		if old == nil {
			return w, fmt.Errorf("disk %d has no partition %d", target.Number, opts.Replace)
		}
		w.Offset, w.Length = old.Offset, old.Length
		if opts.Length > 0 {
			w.Length = alignDown(opts.Length, uint64(target.SectorSize))
		}
		// It may grow into free space directly behind it.
		limit := old.End()
		for _, fr := range freeRanges(target, opts.Replace) {
			if fr[0] <= old.Offset && fr[1] >= old.Offset {
				limit = max(limit, fr[1])
			}
		}
		if w.Offset+w.Length > limit {
			return w, fmt.Errorf("partition %d can grow to at most %d bytes", opts.Replace, limit-w.Offset)
		}
	} else {
		if target.Style == StyleMBR && len(target.Partitions) >= 4 {
			return w, errors.New("the MBR disk already has four partitions")
		}
		w.Length = src.Length
		if opts.Length > 0 {
			w.Length = alignDown(opts.Length, uint64(target.SectorSize))
		}
		found := false
		for _, fr := range freeRanges(target, 0) {
			off := fr[0]
			if opts.Offset > 0 {
				off = opts.Offset
			}
			if off >= fr[0] && off+w.Length <= fr[1] {
				w.Offset, found = off, true
				break
			}
		}
		if !found {
			return w, fmt.Errorf("no free space of %d bytes on disk %d", w.Length, target.Number)
		}
	}
	if target.Style == StyleMBR && w.Offset+w.Length > uint64(mbrMaxSectors)*uint64(target.SectorSize) {
		return w, errors.New("an MBR partition cannot end beyond 2 TiB")
	}
	if w.Length < src.Length {
		if !strings.EqualFold(src.FileSystem, "NTFS") || FixedSizeKind(src) {
			return w, ErrShrinkUnsupported
		}
		w.Shrink = true
	}
	return w, nil
}

// CheckPartitionRestore validates a partition restore against the target
// disk without writing anything.
func CheckPartitionRestore(img *repo.DiskImage, target *Disk, opts PartitionRestoreOptions) error {
	_, err := planPartitionRestore(img, target, opts)
	return err
}

// mbrTypeFor maps a partition to an MBR partition type when it moves from
// a GPT disk.
func mbrTypeFor(p *repo.PartitionImage) byte {
	if p.MBRType != 0 {
		return p.MBRType
	}
	switch strings.ToUpper(p.FileSystem) {
	case "FAT32":
		return 0x0C
	case "FAT", "FAT16":
		return 0x0E
	}
	return 0x07 // NTFS, exFAT, ReFS
}

// gptTypeFor maps a partition to a GPT type when it moves from an MBR disk.
func gptTypeFor(p *repo.PartitionImage) string {
	if p.GPTType != "" {
		return p.GPTType
	}
	if p.MBRType == 0x27 {
		return GPTTypeRecovery
	}
	return GPTTypeBasicData
}

// Sizes and styles in DRIVE_LAYOUT_INFORMATION_EX and
// PARTITION_INFORMATION_EX.
const (
	layoutHeader   = 48
	layoutEntry    = 144
	layoutStyleMBR = 0
	layoutStyleGPT = 1
)

// setLayoutEntry puts the restored partition into a drive layout read
// with IOCTL_DISK_GET_DRIVE_LAYOUT_EX: it replaces the entry of
// partition replace or adds one. It returns the new layout.
func setLayoutEntry(raw []byte, w partitionWrite, replace int, partID string, sectorSize uint32) ([]byte, error) {
	if len(raw) < layoutHeader {
		return nil, errors.New("drive layout too short")
	}
	style := binary.LittleEndian.Uint32(raw[0:])
	count := int(binary.LittleEndian.Uint32(raw[4:]))
	if len(raw) < layoutHeader+count*layoutEntry {
		return nil, errors.New("drive layout truncated")
	}
	out := append([]byte(nil), raw[:layoutHeader+count*layoutEntry]...)
	idx := -1
	for i := 0; i < count; i++ {
		e := out[layoutHeader+i*layoutEntry:]
		num := int(binary.LittleEndian.Uint32(e[24:]))
		if replace > 0 && num == replace {
			idx = i
		}
	}
	if replace > 0 && idx < 0 {
		return nil, fmt.Errorf("partition %d not in the drive layout", replace)
	}
	if idx < 0 && style == layoutStyleMBR {
		// The first unused primary entry.
		for i := 0; i < count && i < 4; i++ {
			e := out[layoutHeader+i*layoutEntry:]
			if e[32] == 0 && binary.LittleEndian.Uint64(e[16:]) == 0 {
				idx = i
				break
			}
		}
		if idx < 0 {
			return nil, errors.New("no free primary partition entry")
		}
	}
	if idx < 0 {
		idx = count
		out = append(out, make([]byte, layoutEntry)...)
		binary.LittleEndian.PutUint32(out[4:], uint32(count+1)) // #nosec G115 -- small count
	}
	e := out[layoutHeader+idx*layoutEntry : layoutHeader+(idx+1)*layoutEntry]
	keepBoot := style == layoutStyleMBR && replace > 0 && e[33] != 0
	for i := range e {
		e[i] = 0
	}
	binary.LittleEndian.PutUint32(e[0:], style)
	binary.LittleEndian.PutUint64(e[8:], w.Offset)
	binary.LittleEndian.PutUint64(e[16:], w.Length)
	e[28] = 1 // RewritePartition
	u := e[32:]
	p := w.Src
	if style == layoutStyleGPT {
		t, err := guidBytesPortable(gptTypeFor(p))
		if err != nil {
			return nil, err
		}
		id, err := guidBytesPortable(partID)
		if err != nil {
			return nil, err
		}
		copy(u[0:16], t[:])
		copy(u[16:32], id[:])
		binary.LittleEndian.PutUint64(u[32:40], p.GPTAttributes)
		name := utf16.Encode([]rune(p.Name))
		for j := 0; j < len(name) && j < 35; j++ {
			binary.LittleEndian.PutUint16(u[40+2*j:], name[j])
		}
	} else {
		u[0] = mbrTypeFor(p)
		if p.Bootable || keepBoot {
			u[1] = 1
		}
		u[2] = 1
		binary.LittleEndian.PutUint32(u[4:8], uint32(w.Offset/uint64(sectorSize))) // #nosec G115 -- MBR offsets are below 2^32 sectors
	}
	return out, nil
}

// guidBytesPortable converts "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx" to the
// on-disk (mixed-endian) GUID layout.
func guidBytesPortable(s string) ([16]byte, error) {
	var out [16]byte
	h := strings.ReplaceAll(strings.Trim(s, "{}"), "-", "")
	if len(h) != 32 {
		return out, fmt.Errorf("invalid GUID %q", s)
	}
	var raw [16]byte
	for i := 0; i < 16; i++ {
		var v byte
		for _, c := range h[2*i : 2*i+2] {
			v <<= 4
			switch {
			case c >= '0' && c <= '9':
				v |= byte(c - '0')
			case c >= 'a' && c <= 'f':
				v |= byte(c-'a') + 10
			case c >= 'A' && c <= 'F':
				v |= byte(c-'A') + 10
			default:
				return out, fmt.Errorf("invalid GUID %q", s)
			}
		}
		raw[i] = v
	}
	// Data1 (4), Data2 (2), Data3 (2) little endian, Data4 as is.
	out[0], out[1], out[2], out[3] = raw[3], raw[2], raw[1], raw[0]
	out[4], out[5] = raw[5], raw[4]
	out[6], out[7] = raw[7], raw[6]
	copy(out[8:], raw[8:])
	return out, nil
}

// partitionIDInUse tells whether a GPT partition ID is used on any disk,
// except by partition skip of disk skipDisk (the one being replaced).
func partitionIDInUse(disks []Disk, id string, skipDisk, skip int) bool {
	for _, d := range disks {
		for _, p := range d.Partitions {
			if d.Number == skipDisk && p.Number == skip {
				continue
			}
			if p.GPTID != "" && strings.EqualFold(p.GPTID, id) {
				return true
			}
		}
	}
	return false
}
