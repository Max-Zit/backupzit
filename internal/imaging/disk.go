// Package imaging backs up and restores whole disks and partitions.
package imaging

import (
	"fmt"
	"strings"
)

// Partition table styles.
const (
	StyleMBR = "mbr"
	StyleGPT = "gpt"
	StyleRaw = "raw"
)

// Well-known GPT partition types.
const (
	GPTTypeEFI       = "C12A7328-F81F-11D2-BA4B-00A0C93EC93B"
	GPTTypeMSR       = "E3C9E316-0B5C-4DB8-817D-F92DF00215AE"
	GPTTypeBasicData = "EBD0A0A2-B9E5-4433-87C0-68B6B72699C7"
	GPTTypeRecovery  = "DE94BBA4-06D1-4D40-A16A-BFD50179D6AC"
)

// Disk describes a physical disk and its partitions.
type Disk struct {
	Number       int         `json:"number"`
	Model        string      `json:"model,omitempty"`
	Size         uint64      `json:"size"`
	SectorSize   uint32      `json:"sector_size"`
	Style        string      `json:"style"`
	GPTDiskID    string      `json:"gpt_disk_id,omitempty"`
	MBRSignature uint32      `json:"mbr_signature,omitempty"`
	System       bool        `json:"system,omitempty"` // holds the running Windows
	Partitions   []Partition `json:"partitions"`
}

// Partition describes one partition and the volume on it, if any.
type Partition struct {
	Number        int    `json:"number"`
	Offset        uint64 `json:"offset"`
	Length        uint64 `json:"length"`
	GPTType       string `json:"gpt_type,omitempty"`
	GPTID         string `json:"gpt_id,omitempty"`
	GPTAttributes uint64 `json:"gpt_attributes,omitempty"`
	Name          string `json:"name,omitempty"`
	MBRType       uint8  `json:"mbr_type,omitempty"`
	Bootable      bool   `json:"bootable,omitempty"`

	VolumeGUIDPath string   `json:"-"`
	MountPoints    []string `json:"mount_points,omitempty"`
	FileSystem     string   `json:"file_system,omitempty"`
	Label          string   `json:"label,omitempty"`
}

// Kind returns a short human description of the partition's role.
func (p Partition) Kind() string {
	switch strings.ToUpper(p.GPTType) {
	case GPTTypeEFI:
		return "EFI System"
	case GPTTypeMSR:
		return "Microsoft Reserved"
	case GPTTypeRecovery:
		return "Recovery"
	case GPTTypeBasicData:
		return "Basic data"
	}
	switch p.MBRType {
	case 0x07:
		return "NTFS/exFAT"
	case 0x0B, 0x0C:
		return "FAT32"
	case 0x27:
		return "Recovery"
	case 0x83:
		return "Linux"
	case 0xEE:
		return "GPT protective"
	}
	if p.GPTType != "" {
		return "GPT " + p.GPTType
	}
	return fmt.Sprintf("type 0x%02x", p.MBRType)
}

// Describe returns e.g. `C: "Windows" NTFS`.
func (p Partition) Describe() string {
	var parts []string
	for _, m := range p.MountPoints {
		parts = append(parts, strings.TrimRight(m, `\`))
	}
	if p.Label != "" {
		parts = append(parts, fmt.Sprintf("%q", p.Label))
	}
	if p.FileSystem != "" {
		parts = append(parts, p.FileSystem)
	}
	parts = append(parts, "("+p.Kind()+")")
	return strings.Join(parts, " ")
}

// End is the first byte after the partition.
func (p Partition) End() uint64 { return p.Offset + p.Length }
