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

// bitmapUsage returns the number of set bits and the index of the last
// set bit (-1 if none) of an NTFS cluster bitmap with n clusters.
func bitmapUsage(bm []byte, n uint64) (used uint64, last int64) {
	last = -1
	full := n / 8
	if full > uint64(len(bm)) {
		full = uint64(len(bm))
	}
	for i := uint64(0); i < full; i++ {
		if c := bm[i]; c != 0 {
			used += uint64(bits.OnesCount8(c))
			last = int64(i*8) + int64(7-bits.LeadingZeros8(c))
		}
	}
	for c := full * 8; c < n && c/8 < uint64(len(bm)); c++ {
		if bm[c/8]&(1<<(c%8)) != 0 {
			used++
			last = int64(c)
		}
	}
	return used, last
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
	if k, err := s.ReadAt(bm, 0); err != nil && !(errors.Is(err, io.EOF) && uint64(k) == uint64(len(bm))) {
		if uint64(k) < uint64(len(bm)) {
			return nil, fmt.Errorf("$Bitmap: read %d of %d bytes: %v", k, len(bm), err)
		}
	}
	used, last := bitmapUsage(bm, n)
	u := &NTFSUsage{Geometry: g, UsedBytes: used * g.ClusterSize}
	// The backup boot sector sits in the last sector of the volume; keep
	// one cluster for it after the last allocated one.
	u.MinSize = alignUp(uint64(last+1)*g.ClusterSize+g.ClusterSize, LayoutAlign)
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
		return PartitionLimits{Reason: "not backed up"}
	case FixedSizeKind(p):
		return PartitionLimits{Reason: Partition{GPTType: p.GPTType, MBRType: p.MBRType}.Kind() + " partitions keep their size"}
	}
	pr, err := NewPartitionReader(ctx, r, p)
	if err != nil {
		return PartitionLimits{Reason: err.Error()}
	}
	if IsBitLockerRaw(pr) {
		return PartitionLimits{Reason: BitLockerRawReason}
	}
	if !strings.EqualFold(p.FileSystem, "NTFS") {
		fs := p.FileSystem
		if fs == "" {
			fs = "unknown file system"
		}
		return PartitionLimits{Reason: fs + " is not resized"}
	}
	u, err := MeasureNTFS(pr)
	if err != nil {
		if errors.Is(err, ErrNotNTFS) {
			return PartitionLimits{Reason: "backed up encrypted (BitLocker) or not readable as NTFS"}
		}
		return PartitionLimits{Reason: err.Error()}
	}
	lim := PartitionLimits{Resizable: true, MinSize: u.MinSize, ShrinkableTo: u.MinSize, UsedBytes: u.UsedBytes}
	// Moving clusters allows less than the last allocated cluster: about
	// the used space. This reads every file record (a while on volumes
	// with many files).
	if vol, err := ntfsresize.Open(pr); err == nil {
		lim.MinSize = min(lim.MinSize, alignUp(vol.MinimumSize(), LayoutAlign))
	}
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
