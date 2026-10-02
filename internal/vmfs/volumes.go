// Package vmfs reads files inside virtual machine disks stored in a backup,
// without restoring the disk: it finds the partitions (GPT, MBR with
// logical partitions), LVM logical volumes and the file systems on them
// (NTFS, ext2/3/4, XFS) and lists and reads their files.
package vmfs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"unicode/utf16"
)

// Volume is a partition or logical volume inside a disk.
type Volume struct {
	ID     string `json:"id"`   // "p1", "p5", "lvm/vg/root"
	Name   string `json:"name"` // human readable
	Offset int64  `json:"-"`
	Size   int64  `json:"size"`
	FS     string `json:"fs"` // ntfs, ext4, xfs, swap, lvm, vfat, ""
	Label  string `json:"label,omitempty"`
	r      io.ReaderAt
}

// Reader returns the volume contents.
func (v Volume) Reader() io.ReaderAt { return v.r }

// Browsable reports whether files of the volume can be listed.
func (v Volume) Browsable() bool { return v.FS == "ntfs" || v.FS == "ext4" || v.FS == "xfs" }

// Volumes finds the partitions and logical volumes of a disk.
func Volumes(disk io.ReaderAt, size int64) ([]Volume, error) {
	head := make([]byte, 64*1024)
	if _, err := disk.ReadAt(head[:min(int64(len(head)), size)], 0); err != nil && err != io.EOF {
		return nil, err
	}
	var parts []Volume
	switch {
	case string(head[512:520]) == "EFI PART":
		p, err := gptPartitions(disk, head)
		if err != nil {
			return nil, err
		}
		parts = p
	case head[510] == 0x55 && head[511] == 0xAA && !looksLikeFS(head):
		parts = mbrPartitions(disk, head, size)
	default:
		parts = []Volume{{ID: "disk", Name: "whole disk", Offset: 0, Size: size}}
	}
	var out []Volume
	for _, p := range parts {
		p.r = io.NewSectionReader(disk, p.Offset, p.Size)
		detect(&p)
		out = append(out, p)
		if p.FS == "lvm" {
			lvs, err := lvmVolumes(p)
			if err != nil {
				out[len(out)-1].Name += fmt.Sprintf(" (LVM: %v)", err)
				continue
			}
			out = append(out, lvs...)
		}
	}
	return out, nil
}

// looksLikeFS reports a file system boot sector (NTFS/FAT also end in 55AA).
func looksLikeFS(head []byte) bool {
	return string(head[3:11]) == "NTFS    " || string(head[54:59]) == "FAT12" || string(head[54:59]) == "FAT16" || string(head[82:87]) == "FAT32"
}

func gptPartitions(disk io.ReaderAt, head []byte) ([]Volume, error) {
	h := head[512:]
	entriesLBA := int64(binary.LittleEndian.Uint64(h[72:]))
	count := int(binary.LittleEndian.Uint32(h[80:]))
	esize := int(binary.LittleEndian.Uint32(h[84:]))
	if esize < 128 || count > 1024 {
		return nil, fmt.Errorf("invalid GPT header")
	}
	buf := make([]byte, count*esize)
	if _, err := disk.ReadAt(buf, entriesLBA*512); err != nil && err != io.EOF {
		return nil, err
	}
	var out []Volume
	for i := 0; i < count; i++ {
		e := buf[i*esize:]
		if bytes.Equal(e[:16], make([]byte, 16)) {
			continue
		}
		first := int64(binary.LittleEndian.Uint64(e[32:]))
		last := int64(binary.LittleEndian.Uint64(e[40:]))
		name := utf16z(e[56:128])
		v := Volume{ID: fmt.Sprintf("p%d", i+1), Offset: first * 512, Size: (last - first + 1) * 512, Label: name}
		v.Name = fmt.Sprintf("Partition %d", i+1)
		out = append(out, v)
	}
	return out, nil
}

func utf16z(b []byte) string {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		c := binary.LittleEndian.Uint16(b[i:])
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return string(utf16.Decode(u))
}

func mbrPartitions(disk io.ReaderAt, head []byte, size int64) []Volume {
	var out []Volume
	for i := 0; i < 4; i++ {
		e := head[446+16*i:]
		typ := e[4]
		start := int64(binary.LittleEndian.Uint32(e[8:])) * 512
		n := int64(binary.LittleEndian.Uint32(e[12:])) * 512
		if typ == 0 || n == 0 || start+n > size+512 {
			continue
		}
		if typ == 0x05 || typ == 0x0f || typ == 0x85 {
			out = append(out, logicalPartitions(disk, start)...)
			continue
		}
		out = append(out, Volume{ID: fmt.Sprintf("p%d", i+1), Name: fmt.Sprintf("Partition %d", i+1), Offset: start, Size: n})
	}
	return out
}

// logicalPartitions walks the chain of extended boot records.
func logicalPartitions(disk io.ReaderAt, ext int64) []Volume {
	var out []Volume
	ebr := ext
	for num := 5; num < 64; num++ {
		b := make([]byte, 512)
		if _, err := disk.ReadAt(b, ebr); err != nil || b[510] != 0x55 || b[511] != 0xAA {
			break
		}
		e1, e2 := b[446:], b[462:]
		if n := int64(binary.LittleEndian.Uint32(e1[12:])) * 512; e1[4] != 0 && n > 0 {
			start := ebr + int64(binary.LittleEndian.Uint32(e1[8:]))*512
			out = append(out, Volume{ID: fmt.Sprintf("p%d", num), Name: fmt.Sprintf("Partition %d (logical)", num), Offset: start, Size: n})
		}
		next := int64(binary.LittleEndian.Uint32(e2[8:])) * 512
		if e2[4] == 0 || next == 0 {
			break
		}
		ebr = ext + next
	}
	return out
}

// detect identifies the file system at the start of a volume.
func detect(v *Volume) {
	b := make([]byte, 8192)
	n, _ := v.r.ReadAt(b, 0)
	b = b[:n]
	switch {
	case len(b) >= 11 && string(b[3:11]) == "NTFS    ":
		v.FS = "ntfs"
	case len(b) >= 4 && string(b[0:4]) == "XFSB":
		v.FS = "xfs"
		v.Label = strings.TrimRight(string(b[108:120]), "\x00")
	case len(b) >= 1024+120 && binary.LittleEndian.Uint16(b[1024+56:]) == 0xEF53:
		v.FS = "ext4"
		v.Label = strings.TrimRight(string(b[1024+120:1024+136]), "\x00")
	case len(b) >= 4096 && string(b[4086:4096]) == "SWAPSPACE2":
		v.FS = "swap"
	case lvmLabel(b) >= 0:
		v.FS = "lvm"
	case len(b) >= 90 && (string(b[82:87]) == "FAT32" || string(b[54:59]) == "FAT16" || string(b[54:59]) == "FAT12"):
		v.FS = "vfat"
	}
}
