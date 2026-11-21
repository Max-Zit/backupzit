package imaging

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/max-zit/backupzit/internal/repo"
)

// LayoutAlign is the alignment of partitions the layout planner creates.
const LayoutAlign = 1 << 20

// mbrMaxSectors is the largest sector number an MBR partition entry can
// address (32-bit LBA).
const mbrMaxSectors = 1 << 32

// PartitionLimits say how a partition of a disk image may change size
// when it is restored.
type PartitionLimits struct {
	// Resizable is false for partitions that can only be moved: no
	// readable file system in the image (BitLocker read raw, unknown file
	// systems), EFI, MSR and recovery partitions, and partitions whose
	// contents were not backed up.
	Resizable bool `json:"resizable"`
	// MinSize is the smallest length the partition can be restored to
	// (its minimum size); 0 means its original length.
	MinSize uint64 `json:"min_size,omitempty"`
	// UsedBytes is the space the file system had in use (informational).
	UsedBytes uint64 `json:"used_bytes,omitempty"`
	// Reason tells why the partition cannot be resized.
	Reason string `json:"reason,omitempty"`
}

// Min returns the smallest allowed length of p.
func (l PartitionLimits) Min(p *repo.PartitionImage) uint64 {
	if !l.Resizable || l.MinSize == 0 {
		return p.Length
	}
	return l.MinSize
}

// LayoutPartition is one partition of a target layout.
type LayoutPartition struct {
	// Source is the number of the partition in the disk image.
	Source int    `json:"source"`
	Offset uint64 `json:"offset"`
	Length uint64 `json:"length"`
	// Restore tells whether the contents are written from the image. A
	// partition that is not restored is created empty.
	Restore bool `json:"restore"`
}

// End is the first byte after the partition.
func (p LayoutPartition) End() uint64 { return p.Offset + p.Length }

// Layout is the target layout of an image restore: which partitions of
// the image go where on the target disk, how large and with which
// partition table style.
type Layout struct {
	Style      string            `json:"style"`
	DiskSize   uint64            `json:"disk_size"`
	SectorSize uint32            `json:"sector_size"`
	Partitions []LayoutPartition `json:"partitions"`
}

// LayoutMode selects how SuggestLayout places partitions.
type LayoutMode int

const (
	// LayoutFit packs the partitions and lets the large resizable
	// partitions (usually Windows) take up the space of the target disk.
	LayoutFit LayoutMode = iota
	// LayoutAsSource keeps the offsets and sizes of the image.
	LayoutAsSource
)

// LayoutOptions control SuggestLayout.
type LayoutOptions struct {
	Mode LayoutMode
	// Restore lists the partition numbers whose contents are written;
	// nil means every included partition.
	Restore []int
	// DropOthers leaves partitions that are not restored out of the
	// layout instead of creating them empty.
	DropOthers bool
}

// ErrTargetTooSmall is wrapped by errors about a too small target disk.
var ErrTargetTooSmall = errors.New("target disk too small")

// TooSmallError reports the disk size a layout would need.
type TooSmallError struct {
	DiskSize uint64
	Needed   uint64
}

func (e *TooSmallError) Error() string {
	return fmt.Sprintf("target disk too small: %d bytes, the partitions need at least %d bytes", e.DiskSize, e.Needed)
}

func (e *TooSmallError) Unwrap() error { return ErrTargetTooSmall }

// FixedSizeKind tells whether a partition keeps its size by its type
// (EFI, MSR, recovery, GPT protective, extended).
func FixedSizeKind(p *repo.PartitionImage) bool {
	switch strings.ToUpper(p.GPTType) {
	case GPTTypeEFI, GPTTypeMSR, GPTTypeRecovery:
		return true
	}
	switch p.MBRType {
	case 0x27, 0xEE, 0xEF, 0x05, 0x0F:
		return true
	}
	return false
}

func alignUp(v, a uint64) uint64   { return (v + a - 1) / a * a }
func alignDown(v, a uint64) uint64 { return v / a * a }

// UsableRange returns the first and the end (exclusive) byte partitions
// may use on a disk of the given style and size.
func UsableRange(style string, diskSize uint64, sectorSize uint32) (first, end uint64) {
	sector := uint64(sectorSize)
	switch style {
	case StyleGPT:
		// Primary GPT: protective MBR + header + 128 entries (34 sectors of
		// 512 bytes, 6 of 4096). The backup GPT takes the last 33 (5).
		entries := alignUp(128*128, sector)
		first = 2*sector + entries
		backup := sector + entries
		if diskSize < backup {
			return first, 0
		}
		end = diskSize - backup
	default:
		first = sector
		end = diskSize
		if lim := uint64(mbrMaxSectors) * sector; end > lim {
			end = lim
		}
	}
	return first, end
}

func findPartition(img *repo.DiskImage, num int) *repo.PartitionImage {
	for i := range img.Partitions {
		if img.Partitions[i].Number == num {
			return &img.Partitions[i]
		}
	}
	return nil
}

func sortedPartitions(img *repo.DiskImage) []*repo.PartitionImage {
	ps := make([]*repo.PartitionImage, 0, len(img.Partitions))
	for i := range img.Partitions {
		ps = append(ps, &img.Partitions[i])
	}
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].Offset < ps[j].Offset })
	return ps
}

// SuggestLayout proposes a target layout for restoring img to a disk of
// diskSize bytes. limits holds the resize limits per partition number;
// partitions without an entry are only moved.
func SuggestLayout(img *repo.DiskImage, diskSize uint64, limits map[int]PartitionLimits, opts LayoutOptions) (*Layout, error) {
	if img.Style != StyleGPT && img.Style != StyleMBR {
		return nil, fmt.Errorf("unsupported partition style %q", img.Style)
	}
	restore := map[int]bool{}
	for _, p := range img.Partitions {
		if opts.Restore == nil {
			restore[p.Number] = p.Included
		}
	}
	for _, n := range opts.Restore {
		p := findPartition(img, n)
		if p == nil {
			return nil, fmt.Errorf("the image has no partition %d", n)
		}
		if !p.Included {
			return nil, fmt.Errorf("partition %d was not backed up", n)
		}
		restore[n] = true
	}
	l := &Layout{Style: img.Style, DiskSize: diskSize, SectorSize: img.SectorSize}
	for _, p := range sortedPartitions(img) {
		if opts.DropOthers && !restore[p.Number] {
			continue
		}
		l.Partitions = append(l.Partitions, LayoutPartition{Source: p.Number, Offset: p.Offset, Length: p.Length, Restore: restore[p.Number]})
	}
	if len(l.Partitions) == 0 {
		return nil, errors.New("no partitions to restore")
	}
	if opts.Mode == LayoutAsSource {
		return l, l.Validate(img, limits)
	}
	if err := l.fit(img, limits); err != nil {
		return nil, err
	}
	return l, l.Validate(img, limits)
}

// absorbers returns the indexes of the partitions that take up free or
// missing space: the large resizable ones (at least a quarter of all
// resizable space). With a single one, that is usually Windows.
func (l *Layout) absorbers(img *repo.DiskImage, limits map[int]PartitionLimits) []int {
	var total uint64
	var res []int
	for i, lp := range l.Partitions {
		if limits[lp.Source].Resizable && lp.Restore {
			total += findPartition(img, lp.Source).Length
			res = append(res, i)
		}
	}
	var out []int
	for _, i := range res {
		if findPartition(img, l.Partitions[i].Source).Length*4 >= total {
			out = append(out, i)
		}
	}
	return out
}

// fit packs the partitions from the first usable, aligned offset and
// distributes the remaining space over the absorbing partitions in
// proportion to their original size, never below their minimum size.
func (l *Layout) fit(img *repo.DiskImage, limits map[int]PartitionLimits) error {
	first, end := UsableRange(l.Style, l.DiskSize, l.SectorSize)
	start := alignUp(first, LayoutAlign)
	end = alignDown(end, LayoutAlign)
	// Keep a source that starts earlier than 1 MiB (e.g. 63 sectors on old
	// MBR disks) where it was, if that is valid on the target.
	if src := l.Partitions[0].Offset; src >= first && src < start {
		start = src
	}

	abs := l.absorbers(img, limits)
	isAbs := map[int]bool{}
	for _, i := range abs {
		isAbs[i] = true
	}
	// Space the other partitions need, packed and aligned.
	var fixed uint64
	for i, lp := range l.Partitions {
		if !isAbs[i] {
			fixed += alignUp(lp.Length, LayoutAlign)
		}
	}
	var minAbs uint64
	for _, i := range abs {
		minAbs += alignUp(limits[l.Partitions[i].Source].Min(findPartition(img, l.Partitions[i].Source)), LayoutAlign)
	}
	needed := start + fixed + minAbs
	if l.Style == StyleGPT {
		_, e := UsableRange(StyleGPT, needed, l.SectorSize)
		needed += needed - e
	}
	if end < start || end-start < fixed+minAbs {
		return &TooSmallError{DiskSize: l.DiskSize, Needed: alignUp(needed, LayoutAlign)}
	}

	// Distribute: proportional to the original length, clamped to the
	// minimum; partitions that hit their minimum are taken out and the
	// rest is shared again.
	avail := end - start - fixed
	lengths := map[int]uint64{}
	open := append([]int(nil), abs...)
	for len(open) > 0 {
		var srcTotal uint64
		for _, i := range open {
			srcTotal += findPartition(img, l.Partitions[i].Source).Length
		}
		var clamped []int
		for _, i := range open {
			src := findPartition(img, l.Partitions[i].Source)
			share := alignDown(uint64(float64(avail)*float64(src.Length)/float64(srcTotal)), LayoutAlign)
			if mn := alignUp(limits[src.Number].Min(src), LayoutAlign); share < mn {
				clamped = append(clamped, i)
				lengths[i] = mn
			}
		}
		if len(clamped) == 0 {
			var used uint64
			for k, i := range open {
				src := findPartition(img, l.Partitions[i].Source)
				share := alignDown(uint64(float64(avail)*float64(src.Length)/float64(srcTotal)), LayoutAlign)
				if k == len(open)-1 {
					share = alignDown(avail-used, LayoutAlign)
				}
				lengths[i] = share
				used += share
			}
			break
		}
		for _, i := range clamped {
			avail -= lengths[i]
			for k, j := range open {
				if j == i {
					open = append(open[:k], open[k+1:]...)
					break
				}
			}
		}
	}
	for i := range l.Partitions {
		if n, ok := lengths[i]; ok {
			l.Partitions[i].Length = n
		}
	}
	l.pack(0, start)
	return nil
}

// pack places partitions from index i on one after the other, each on
// the next aligned offset, starting at off.
func (l *Layout) pack(i int, off uint64) {
	for ; i < len(l.Partitions); i++ {
		l.Partitions[i].Offset = off
		off = alignUp(l.Partitions[i].End(), LayoutAlign)
	}
}

// Resize sets the length of the partition restored from image partition
// num (rounded down to whole sectors) and moves the partitions after it
// so they follow directly. Validate the layout afterwards.
func (l *Layout) Resize(num int, length uint64) error {
	for i := range l.Partitions {
		if l.Partitions[i].Source == num {
			l.Partitions[i].Length = alignDown(length, uint64(l.SectorSize))
			l.pack(i+1, alignUp(l.Partitions[i].End(), LayoutAlign))
			return nil
		}
	}
	return fmt.Errorf("partition %d is not in the layout", num)
}

// Validate checks that the layout fits the disk, keeps the order and the
// limits of each partition and can be written in its partition style.
func (l *Layout) Validate(img *repo.DiskImage, limits map[int]PartitionLimits) error {
	if l.SectorSize != img.SectorSize {
		return fmt.Errorf("target disk has %d byte sectors, image was taken from %d byte sectors", l.SectorSize, img.SectorSize)
	}
	if l.Style != StyleGPT && l.Style != StyleMBR {
		return fmt.Errorf("unsupported partition style %q", l.Style)
	}
	if l.Style == StyleMBR && len(l.Partitions) > 4 {
		return fmt.Errorf("an MBR disk holds at most 4 partitions, the layout has %d", len(l.Partitions))
	}
	sector := uint64(l.SectorSize)
	first, end := UsableRange(l.Style, l.DiskSize, l.SectorSize)
	seen := map[int]bool{}
	var prevEnd uint64
	var last uint64
	for i, lp := range l.Partitions {
		src := findPartition(img, lp.Source)
		if src == nil {
			return fmt.Errorf("the image has no partition %d", lp.Source)
		}
		if seen[lp.Source] {
			return fmt.Errorf("partition %d is in the layout twice", lp.Source)
		}
		seen[lp.Source] = true
		if lp.Restore && !src.Included {
			return fmt.Errorf("partition %d was not backed up", lp.Source)
		}
		if lp.Offset%sector != 0 || lp.Length%sector != 0 || lp.Length == 0 {
			return fmt.Errorf("partition %d: offset and length must be whole sectors", lp.Source)
		}
		if i > 0 && lp.Offset < prevEnd {
			return fmt.Errorf("partition %d overlaps the partition before it", lp.Source)
		}
		if lp.Offset < first {
			return fmt.Errorf("partition %d starts inside the partition table", lp.Source)
		}
		prevEnd = lp.End()
		last = max(last, lp.End())
		lim := limits[lp.Source]
		switch {
		case !lim.Resizable && lp.Length != src.Length:
			why := lim.Reason
			if why == "" {
				why = "it can only be moved"
			}
			return fmt.Errorf("partition %d cannot be resized: %s", lp.Source, why)
		case lp.Length < lim.Min(src):
			return fmt.Errorf("partition %d needs at least %d bytes, the layout gives it %d", lp.Source, lim.Min(src), lp.Length)
		}
	}
	if last > end {
		needed := last
		if l.Style == StyleGPT {
			_, e := UsableRange(StyleGPT, last, l.SectorSize)
			needed += last - e
		}
		if l.Style == StyleMBR && last > uint64(mbrMaxSectors)*sector {
			return fmt.Errorf("an MBR disk can only address %d bytes; use GPT", uint64(mbrMaxSectors)*sector)
		}
		return &TooSmallError{DiskSize: l.DiskSize, Needed: needed}
	}
	return nil
}

// Moved tells whether any restored partition changes offset or size, so
// file systems need adjusting and boot files rebuilding afterwards.
func (l *Layout) Moved(img *repo.DiskImage) bool {
	for _, lp := range l.Partitions {
		src := findPartition(img, lp.Source)
		if src == nil || src.Offset != lp.Offset || src.Length != lp.Length {
			return true
		}
	}
	return false
}

// NeededDiskSize is the smallest disk a fitted layout of img needs.
func NeededDiskSize(img *repo.DiskImage, limits map[int]PartitionLimits, opts LayoutOptions) (uint64, error) {
	opts.Mode = LayoutFit
	_, err := SuggestLayout(img, 0, limits, opts)
	var tse *TooSmallError
	if errors.As(err, &tse) {
		return tse.Needed, nil
	}
	return 0, err
}
