//go:build windows

package imaging

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/vss"
	"golang.org/x/sys/windows"
)

// BlockSize is the unit of image deduplication.
const BlockSize = 1 << 20

// BackupOptions configure an image backup.
type BackupOptions struct {
	Disk int
	// Partitions selects partition numbers to back up; nil means all.
	Partitions []int
	// VSS reads NTFS/ReFS volumes from shadow copies (recommended).
	VSS        bool
	VSSTimeout time.Duration
	Hostname   string
	Version    string
	Tags       []string
	// Progress is called with bytes processed and total bytes to process.
	Progress func(done, total uint64)
}

func included(sel []int, n int) bool {
	if sel == nil {
		return true
	}
	for _, s := range sel {
		if s == n {
			return true
		}
	}
	return false
}

// Backup images a disk (or, with Disk = AllDisks, every internal disk)
// into the repository and saves one snapshot. All volumes of all disks are
// read from one VSS snapshot set, so the disks are consistent with each
// other.
func Backup(ctx context.Context, r *repo.Repository, opts BackupOptions) (*repo.Snapshot, error) {
	all, err := ListDisks()
	if err != nil {
		return nil, err
	}
	var disks []*Disk
	if opts.Disk == AllDisks {
		for i := range all {
			if InAllDisks(all[i]) {
				disks = append(disks, &all[i])
			}
		}
		if len(disks) == 0 {
			return nil, errors.New("no internal disk with a partition table found")
		}
		opts.Partitions = nil
	} else {
		var disk *Disk
		for i := range all {
			if all[i].Number == opts.Disk {
				disk = &all[i]
			}
		}
		if disk == nil {
			return nil, fmt.Errorf("disk %d not found", opts.Disk)
		}
		if disk.Style == StyleRaw {
			return nil, fmt.Errorf("disk %d has no partition table", opts.Disk)
		}
		for _, n := range opts.Partitions {
			found := false
			for _, p := range disk.Partitions {
				found = found || p.Number == n
			}
			if !found {
				return nil, fmt.Errorf("disk %d has no partition %d", opts.Disk, n)
			}
		}
		disks = []*Disk{disk}
	}
	if opts.Hostname == "" {
		opts.Hostname, _ = os.Hostname()
	}

	start := time.Now()
	before := r.Stats()
	var stats repo.SnapshotStats
	addErr := func(item string, err error) { stats.Errors = append(stats.Errors, fmt.Sprintf("%s: %v", item, err)) }

	// Snapshot every included volume that VSS supports, of all disks at once.
	var snaps *vss.Set
	if opts.VSS {
		var vols []string
		for _, disk := range disks {
			for _, p := range disk.Partitions {
				if included(opts.Partitions, p.Number) && p.VolumeGUIDPath != "" && vssCapable(p.FileSystem) {
					vols = append(vols, p.VolumeGUIDPath)
				}
			}
		}
		timeout := opts.VSSTimeout
		if timeout <= 0 {
			timeout = 5 * time.Minute
		}
		snaps = vss.CreateVolumes(vols, timeout, func(item string, err error) {
			addErr(item, fmt.Errorf("VSS snapshot failed, reading live: %w", err))
		})
		defer snaps.Close()
	}

	var total, done uint64
	for _, disk := range disks {
		for _, p := range disk.Partitions {
			if included(opts.Partitions, p.Number) {
				total += p.Length
			}
		}
	}
	progress := func(n uint64) {
		done += n
		if opts.Progress != nil {
			opts.Progress(done, total)
		}
	}

	sn := &repo.Snapshot{
		Time:           start.UTC(),
		Hostname:       opts.Hostname,
		Tags:           append([]string{"image"}, opts.Tags...),
		ProgramVersion: opts.Version,
	}
	for _, disk := range disks {
		img, err := imageDisk(ctx, r, disk, opts.Partitions, snaps, progress, addErr, &stats)
		if err != nil {
			return nil, err
		}
		sn.Paths = append(sn.Paths, physicalDrivePath(disk.Number))
		sn.Images = append(sn.Images, img)
	}

	// The snapshot has no file tree; store an empty one so all snapshots
	// share the same shape.
	treeID, err := r.SaveTree(ctx, &repo.Tree{})
	if err != nil {
		return nil, err
	}
	if err := r.Flush(ctx); err != nil {
		return nil, err
	}
	if err := snaps.Close(); err != nil {
		addErr("vss", fmt.Errorf("delete snapshot: %w", err))
	}
	after := r.Stats()
	stats.BytesAdded = after.RawBytes - before.RawBytes
	stats.BytesStored = after.StoredBytes - before.StoredBytes
	stats.Duration = time.Since(start)
	sn.Tree, sn.Stats = treeID, stats
	if snaps != nil {
		for _, img := range sn.Images {
			for _, p := range img.Partitions {
				if p.Source == "vss" {
					sn.VSSVolumes = append(sn.VSSVolumes, partitionLabel(p))
				}
			}
		}
	}
	if _, err := r.SaveSnapshot(ctx, sn); err != nil {
		return nil, fmt.Errorf("save snapshot: %w", err)
	}
	return sn, nil
}

// imageDisk stores the head (partition table, boot code) and the selected
// partitions of one disk.
func imageDisk(ctx context.Context, r *repo.Repository, disk *Disk, parts []int, snaps *vss.Set,
	progress func(uint64), addErr func(string, error), stats *repo.SnapshotStats) (repo.DiskImage, error) {

	img := repo.DiskImage{
		Number: disk.Number, Model: disk.Model, Size: disk.Size, SectorSize: disk.SectorSize,
		Style: disk.Style, GPTDiskID: disk.GPTDiskID, MBRSignature: disk.MBRSignature,
	}
	diskH, err := openDevice(physicalDrivePath(disk.Number), false)
	if err != nil {
		return img, fmt.Errorf("open disk %d: %w", disk.Number, err)
	}
	diskF := os.NewFile(uintptr(diskH), physicalDrivePath(disk.Number))
	defer diskF.Close()

	head := make([]byte, min(uint64(BlockSize), disk.Size))
	if _, err := diskF.ReadAt(head, 0); err != nil {
		return img, fmt.Errorf("disk %d: read disk head: %w", disk.Number, err)
	}
	if img.Head, _, err = r.SaveBlob(ctx, repo.DataBlob, head); err != nil {
		return img, err
	}
	for _, p := range disk.Partitions {
		pi := repo.PartitionImage{
			Number: p.Number, Offset: p.Offset, Length: p.Length,
			GPTType: p.GPTType, GPTID: p.GPTID, GPTAttributes: p.GPTAttributes, Name: p.Name,
			MBRType: p.MBRType, Bootable: p.Bootable,
			MountPoints: p.MountPoints, FileSystem: p.FileSystem, Label: p.Label,
		}
		if !included(parts, p.Number) {
			img.Partitions = append(img.Partitions, pi)
			continue
		}
		if err := imagePartition(ctx, r, diskF, p, snaps, &pi, progress, addErr); err != nil {
			return img, fmt.Errorf("disk %d partition %d: %w", disk.Number, p.Number, err)
		}
		stats.Files++
		stats.Bytes += pi.StoredBytes
		stats.BytesRead += pi.StoredBytes
		img.Partitions = append(img.Partitions, pi)
	}
	return img, nil
}

func partitionLabel(p repo.PartitionImage) string {
	if len(p.MountPoints) > 0 {
		return strings.TrimRight(p.MountPoints[0], `\`)
	}
	return fmt.Sprintf("partition %d", p.Number)
}

func vssCapable(fs string) bool {
	return strings.EqualFold(fs, "NTFS") || strings.EqualFold(fs, "ReFS")
}

// imagePartition reads one partition block by block and stores it.
func imagePartition(ctx context.Context, r *repo.Repository, disk *os.File, p Partition, snaps *vss.Set,
	pi *repo.PartitionImage, progress func(uint64), addErr func(string, error)) error {

	pi.Included = true
	pi.BlockSize = BlockSize
	pi.Blocks = (p.Length + BlockSize - 1) / BlockSize

	// Choose where to read from: shadow copy, live volume, or raw disk.
	var src io.ReaderAt = io.NewSectionReader(disk, int64(p.Offset), int64(p.Length))
	pi.Source = "live"
	var volH windows.Handle
	if dev, ok := snaps.Device(p.VolumeGUIDPath); ok {
		h, err := openDevice(dev, false)
		if err != nil {
			addErr(partitionLabel(*pi), fmt.Errorf("open shadow copy, reading live: %w", err))
		} else {
			volH, pi.Source = h, "vss"
		}
	}
	if volH != 0 {
		ioctl(volH, fsctlAllowExtendedDasdIO, nil, 0)
		f := os.NewFile(uintptr(volH), "volume")
		defer f.Close()
		src = f
	}

	// Find allocated blocks for NTFS; read everything otherwise.
	used, err := usedBlocks(volH, p, pi.Source == "vss" && strings.EqualFold(p.FileSystem, "NTFS"))
	if err != nil {
		addErr(partitionLabel(*pi), fmt.Errorf("cluster bitmap unavailable, imaging all blocks: %w", err))
		used = nil
	}
	if used != nil {
		pi.Method = "used-blocks"
	} else {
		pi.Method = "full"
	}

	ids := make([]repo.ID, pi.Blocks)
	buf := make([]byte, BlockSize)
	for i := uint64(0); i < pi.Blocks; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := min(uint64(BlockSize), p.Length-i*BlockSize)
		if used != nil && !used[i] {
			progress(n)
			continue
		}
		if _, err := src.ReadAt(buf[:n], int64(i*BlockSize)); err != nil {
			return fmt.Errorf("read block %d at %d: %w", i, i*BlockSize, err)
		}
		id, _, err := r.SaveBlob(ctx, repo.DataBlob, buf[:n])
		if err != nil {
			return err
		}
		ids[i] = id
		pi.StoredBytes += n
		progress(n)
	}
	pi.Maps, err = r.SaveBlockMap(ctx, ids)
	return err
}

// usedBlocks returns which BlockSize blocks of an NTFS volume contain
// allocated clusters, or nil if all blocks must be imaged.
func usedBlocks(vol windows.Handle, p Partition, ntfs bool) ([]bool, error) {
	if vol == 0 || !ntfs {
		return nil, nil
	}
	nv, err := ioctl(vol, fsctlGetNtfsVolumeData, nil, 512)
	if err != nil {
		return nil, fmt.Errorf("ntfs volume data: %w", err)
	}
	totalClusters := binary.LittleEndian.Uint64(nv[16:24])
	clusterSize := uint64(binary.LittleEndian.Uint32(nv[44:48]))
	if clusterSize == 0 || BlockSize%clusterSize != 0 {
		return nil, fmt.Errorf("unsupported cluster size %d", clusterSize)
	}
	blocks := (p.Length + BlockSize - 1) / BlockSize
	used := make([]bool, blocks)
	perBlock := BlockSize / clusterSize

	var lcn uint64
	for lcn < totalClusters {
		in := make([]byte, 8)
		binary.LittleEndian.PutUint64(in, lcn)
		out, err := ioctlPartial(vol, fsctlGetVolumeBitmap, in, 1<<20)
		if err != nil {
			return nil, fmt.Errorf("volume bitmap: %w", err)
		}
		start := binary.LittleEndian.Uint64(out[0:8])
		count := binary.LittleEndian.Uint64(out[8:16])
		bits := out[16:]
		avail := min(count, uint64(len(bits))*8)
		for c := uint64(0); c < avail; c++ {
			if bits[c/8]&(1<<(c%8)) != 0 {
				used[(start+c)/perBlock] = true
			}
		}
		if avail == 0 {
			break
		}
		lcn = start + avail
	}
	// Everything after the last cluster (NTFS keeps its backup boot sector
	// there) is always imaged.
	for b := totalClusters * clusterSize / BlockSize; b < blocks; b++ {
		used[b] = true
	}
	return used, nil
}

// ioctlPartial is like ioctl but returns partial output on ERROR_MORE_DATA,
// as FSCTL_GET_VOLUME_BITMAP delivers large bitmaps in pieces.
func ioctlPartial(h windows.Handle, code uint32, in []byte, outSize int) ([]byte, error) {
	out := make([]byte, outSize)
	var n uint32
	err := windows.DeviceIoControl(h, code, &in[0], uint32(len(in)), &out[0], uint32(outSize), &n, nil)
	if err != nil && !errors.Is(err, windows.ERROR_MORE_DATA) {
		return nil, err
	}
	return out[:n], nil
}
