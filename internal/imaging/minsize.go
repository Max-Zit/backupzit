package imaging

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"strings"

	"github.com/max-zit/backupzit/internal/imaging/ntfsresize"
	"github.com/max-zit/backupzit/internal/repo"
	ntfs "www.velocidex.com/golang/go-ntfs/parser"
)

// NTFSGeometry is what the NTFS boot sector says about a volume.
type NTFSGeometry struct {
	BytesPerSector uint32
	ClusterSize    uint64
	TotalSectors   uint64
	MFTCluster     uint64
	MFTMirrCluster uint64
}

// Clusters is the number of clusters of the volume.
func (g NTFSGeometry) Clusters() uint64 {
	return g.TotalSectors * uint64(g.BytesPerSector) / g.ClusterSize
}

// ParseNTFSBoot reads the geometry from an NTFS boot sector.
func ParseNTFSBoot(b []byte) (NTFSGeometry, error) {
	var g NTFSGeometry
	if len(b) < 512 || string(b[3:11]) != "NTFS    " {
		return g, ErrNotNTFS
	}
	g.BytesPerSector = uint32(binary.LittleEndian.Uint16(b[0x0B:]))
	spc := uint64(b[0x0D])
	if spc > 0x80 {
		// Clusters of 128 KiB and more: 2^(256-x) sectors.
		spc = 1 << (256 - spc)
	}
	g.ClusterSize = spc * uint64(g.BytesPerSector)
	g.TotalSectors = binary.LittleEndian.Uint64(b[0x28:])
	g.MFTCluster = binary.LittleEndian.Uint64(b[0x30:])
	g.MFTMirrCluster = binary.LittleEndian.Uint64(b[0x38:])
	if g.BytesPerSector < 256 || g.BytesPerSector&(g.BytesPerSector-1) != 0 || g.ClusterSize == 0 || g.TotalSectors == 0 {
		return g, fmt.Errorf("%w: implausible boot sector", ErrNotNTFS)
	}
	return g, nil
}

// bitmapUsage returns the number of set bits of an NTFS cluster bitmap
// with n clusters and the end of the used area: one past the last set bit
// (0 if none). Unsigned throughout, so no conversion can overflow.
func bitmapUsage(bm []byte, n uint64) (used, end uint64) {
	full := min(n/8, uint64(len(bm)))
	for i := uint64(0); i < full; i++ {
		if c := bm[i]; c != 0 {
			used += onesCount8(c)
			end = i*8 + 8 - leadingZeros8(c)
		}
	}
	for c := full * 8; c < n && c/8 < uint64(len(bm)); c++ {
		if bm[c/8]&(1<<(c%8)) != 0 {
			used++
			end = c + 1
		}
	}
	return used, end
}

// onesCount8 is bits.OnesCount8 as a uint64.
func onesCount8(c byte) uint64 {
	return uint64(bits.OnesCount8(c)) // #nosec G115 -- OnesCount8 returns 0..8
}

// leadingZeros8 is bits.LeadingZeros8 as a uint64.
func leadingZeros8(c byte) uint64 {
	return uint64(bits.LeadingZeros8(c)) // #nosec G115 -- LeadingZeros8 returns 0..8
}

// NTFSUsage describes how much of an NTFS volume is in use.
type NTFSUsage struct {
	Geometry NTFSGeometry
	// UsedBytes is the space of all allocated clusters.
	UsedBytes uint64
	// MinSize is the smallest partition length that keeps every
	// allocated cluster without moving it: up to the last allocated
	// cluster plus the backup boot sector, rounded up to LayoutAlign.
	MinSize uint64
}

// MeasureNTFS reads the boot sector and the cluster bitmap ($Bitmap) of
// an NTFS volume.
func MeasureNTFS(r io.ReaderAt) (*NTFSUsage, error) {
	boot := make([]byte, 512)
	if _, err := r.ReadAt(boot, 0); err != nil {
		return nil, fmt.Errorf("read boot sector: %w", err)
	}
	g, err := ParseNTFSBoot(boot)
	if err != nil {
		return nil, err
	}
	paged, err := ntfs.NewPagedReader(r, 1024*1024, 64)
	if err != nil {
		return nil, err
	}
	c, err := ntfs.GetNTFSContext(paged, 0)
	if err != nil {
		return nil, fmt.Errorf("read NTFS: %w", err)
	}
	defer c.Close()
	e, err := c.GetMFT(6) // $Bitmap
	if err != nil {
		return nil, fmt.Errorf("$Bitmap: %w", err)
	}
	s, err := ntfs.OpenStream(c, e, ntfs.ATTR_TYPE_DATA, ntfs.WILDCARD_STREAM_ID, ntfs.WILDCARD_STREAM_NAME)
	if err != nil {
		return nil, fmt.Errorf("$Bitmap: %w", err)
	}
	n := g.Clusters()
	bm := make([]byte, (n+7)/8)
	if k, err := s.ReadAt(bm, 0); err != nil && k < len(bm) {
		return nil, fmt.Errorf("$Bitmap: read %d of %d bytes: %v", k, len(bm), err)
	}
	used, end := bitmapUsage(bm, n)
	u := &NTFSUsage{Geometry: g, UsedBytes: used * g.ClusterSize}
	// The backup boot sector sits in the last sector of the volume; keep
	// one cluster for it after the last allocated one.
	u.MinSize = alignUp(end*g.ClusterSize+g.ClusterSize, LayoutAlign)
	if vol := g.TotalSectors * uint64(g.BytesPerSector); u.MinSize > vol+uint64(g.BytesPerSector) {
		u.MinSize = vol + uint64(g.BytesPerSector)
	}
	return u, nil
}

// LimitsForPartition works out how far an image partition can be resized.
// The file system is read from the repository, which loads a few blocks.
func LimitsForPartition(ctx context.Context, r *repo.Repository, p *repo.PartitionImage) PartitionLimits {
	switch {
	case !p.Included:
		return PartitionLimits{Reason: "not backed up", Why: ReasonNotBackedUp}
	case FixedSizeKind(p):
		return PartitionLimits{Reason: Partition{GPTType: p.GPTType, MBRType: p.MBRType}.Kind() + " partitions keep their size", Why: ReasonFixedSize}
	}
	pr, err := NewPartitionReader(ctx, r, p)
	if err != nil {
		return PartitionLimits{Reason: err.Error(), Why: ReasonError}
	}
	if IsBitLockerRaw(pr) {
		return PartitionLimits{Reason: BitLockerRawReason, Why: ReasonBitLocker}
	}
	if !strings.EqualFold(p.FileSystem, "NTFS") {
		fs := p.FileSystem
		if fs == "" {
			fs = "unknown file system"
		}
		return PartitionLimits{Reason: fs + " is not resized", Why: ReasonFileSystem}
	}
	u, err := MeasureNTFS(pr)
	if err != nil {
		if errors.Is(err, ErrNotNTFS) {
			return PartitionLimits{Reason: "backed up encrypted (BitLocker) or not readable as NTFS", Why: ReasonNotNTFS}
		}
		return PartitionLimits{Reason: err.Error(), Why: ReasonError}
	}
	return shrinkLimits(pr, p, u)
}

// ShrinkOptions are the ntfsresize planning options for a partition
// image. Only an image read from a VSS shadow copy may have data moved:
// VSS made it consistent, and its $LogFile is open because the volume
// was mounted. Any other image (read live, offline, or of unknown
// source) is shrunk only down to its last used cluster, and its $LogFile
// must say it was shut down cleanly. Restore planning and the limits
// shown before it use the same options, so the limits hold.
func ShrinkOptions(p *repo.PartitionImage) ntfsresize.PlanOptions {
	vss := p.Source == "vss"
	return ntfsresize.PlanOptions{FromSnapshot: vss, NoMove: !vss}
}

// shrinkLimits works out how far an NTFS partition image can shrink, from
// the bitmap measures in u and a planning pass over every file record (a
// while on volumes with many files). MinSize is only a size the restore's
// planning accepts; when it would refuse every shrink (an inconsistent
// or unclean source), the partition keeps the size its volume needs and
// Reason says why (Why is ReasonNoShrink; it can still grow).
func shrinkLimits(pr io.ReaderAt, p *repo.PartitionImage, u *NTFSUsage) PartitionLimits {
	lim := PartitionLimits{Resizable: true, MinSize: u.MinSize, ShrinkableTo: u.MinSize, UsedBytes: u.UsedBytes}
	vol, err := ntfsresize.Open(pr)
	if err != nil {
		lim.MinSize, lim.ShrinkableTo = p.Length, p.Length
		lim.Reason, lim.Why = NoShrinkPrefix+err.Error(), ReasonNoShrink
		return lim
	}
	m, err := vol.MinimumSize(ShrinkOptions(p))
	if err != nil {
		// A partition at least as large as the volume restores it as it
		// is, which needs none of the checks.
		keep := min(p.Length, alignUp(vol.UnchangedSize(), LayoutAlign))
		lim.MinSize, lim.ShrinkableTo = keep, keep
		lim.Reason, lim.Why = NoShrinkPrefix+err.Error(), ReasonNoShrink
		return lim
	}
	// Every size from the minimum up is accepted, so aligning up keeps it.
	lim.MinSize = min(alignUp(m, LayoutAlign), p.Length)
	lim.ShrinkableTo = max(lim.MinSize, min(lim.ShrinkableTo, p.Length))
	return lim
}

// BitLockerRawReason explains why a partition backed up encrypted cannot
// be resized.
const BitLockerRawReason = "backed up encrypted with BitLocker: it can only be moved and needs its recovery key after the restore"

// IsBitLockerRaw tells whether a partition image starts with a BitLocker
// volume header ("-FVE-FS-"): it was read encrypted, not through a
// shadow copy of the unlocked volume.
func IsBitLockerRaw(r io.ReaderAt) bool {
	b := make([]byte, 16)
	if _, err := r.ReadAt(b, 0); err != nil {
		return false
	}
	return string(b[3:11]) == "-FVE-FS-"
}

// ImageLimits returns the resize limits of every partition of a disk image.
func ImageLimits(ctx context.Context, r *repo.Repository, img *repo.DiskImage) map[int]PartitionLimits {
	out := map[int]PartitionLimits{}
	for i := range img.Partitions {
		p := &img.Partitions[i]
		out[p.Number] = LimitsForPartition(ctx, r, p)
	}
	return out
}

// HasWindows tells whether an image partition holds a Windows
// installation (its SYSTEM registry hive).
func HasWindows(ctx context.Context, r *repo.Repository, p *repo.PartitionImage) bool {
	if !p.Included || !strings.EqualFold(p.FileSystem, "NTFS") {
		return false
	}
	v, err := OpenVolume(ctx, r, p)
	if err != nil {
		return false
	}
	_, err = v.Stat(`\Windows\System32\config\SYSTEM`)
	return err == nil
}
