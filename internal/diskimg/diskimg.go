// Package diskimg stores whole virtual disks in a repository as fixed-size
// blocks (unchanged blocks are deduplicated, empty ones not stored) and
// writes them back. It is shared by the hypervisor integrations.
package diskimg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/max-zit/backupzit/internal/repo"
)

// BlockSize is the historical default and maximum VM block size.
// New images use the session setting; readers use the image metadata.
const BlockSize = 1 << 20

var zeroBlock = make([]byte, BlockSize)

// Store reads a disk of the given size and saves it as a DiskImage with one
// whole-disk partition. All-zero blocks are recorded as null IDs.
func Store(ctx context.Context, r *repo.Repository, f io.ReaderAt, size uint64, number int, model, source string, progress func(uint64)) (repo.DiskImage, error) {
	img, err := StoreAreas(ctx, r, f, size, []Area{{Offset: 0, Length: size}}, nil, number, model, source, progress)
	if err == nil {
		img.Partitions[0].Method = "full"
	}
	return img, err
}

// Write writes a disk image saved by Store to w. Null blocks are skipped
// unless writeZeros is set (targets that may hold old data).
func Write(ctx context.Context, r *repo.Repository, img *repo.DiskImage, w io.WriterAt, writeZeros bool, progress func(uint64)) error {
	if len(img.Partitions) != 1 {
		return errors.New("not a whole-disk image")
	}
	if progress == nil {
		progress = func(uint64) {}
	}
	p := &img.Partitions[0]
	ids, err := r.LoadBlockMap(ctx, p)
	if err != nil {
		return err
	}
	for i, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		off := uint64(i) * uint64(p.BlockSize)
		n := min(uint64(p.BlockSize), p.Length-off)
		if id.IsNull() {
			if writeZeros {
				if _, err := w.WriteAt(zeroBlock[:n], int64(off)); err != nil { // #nosec G115 -- disk offsets are below 2^63
					return err
				}
			}
			progress(n)
			continue
		}
		b, err := r.LoadBlob(ctx, repo.DataBlob, id)
		if err != nil {
			return fmt.Errorf("block %d: %w", i, err)
		}
		if uint64(len(b)) != n {
			return fmt.Errorf("block %d has %d bytes, expected %d", i, len(b), n)
		}
		if _, err := w.WriteAt(b, int64(off)); err != nil { // #nosec G115 -- disk offsets are below 2^63
			return fmt.Errorf("write block %d: %w", i, err)
		}
		progress(n)
	}
	return nil
}

// Area is a byte range of a disk.
type Area struct{ Offset, Length uint64 }

// StoreAreas saves a disk like Store but reads only the blocks that overlap
// areas (the allocated or changed regions reported by changed block
// tracking). Blocks outside the areas are taken from prev, the image of the
// same disk in an earlier backup, or are empty when prev is nil.
func StoreAreas(ctx context.Context, r *repo.Repository, f io.ReaderAt, size uint64, areas []Area, prev *repo.DiskImage,
	number int, model, source string, progress func(uint64)) (repo.DiskImage, error) {
	if size == 0 {
		return repo.DiskImage{}, errors.New("disk is empty")
	}
	blockSize := uint64(r.VMBlockSizeKiB()) << 10
	// A changed-block map is only meaningful with its original block size.
	// Re-read the whole disk once when the job switches size, even if the
	// hypervisor supplied only dirty ranges. Never treat the rest as zeros.
	reblocked := false
	if prev != nil {
		if prev.Size != size || len(prev.Partitions) != 1 {
			return repo.DiskImage{}, errors.New("the earlier backup of this disk has a different size")
		}
		if uint64(prev.Partitions[0].BlockSize) != blockSize {
			prev = nil
			areas = []Area{{Offset: 0, Length: size}}
			reblocked = true
		}
	}
	if progress == nil {
		progress = func(uint64) {}
	}
	img := repo.DiskImage{Number: number, Model: model, Size: size, SectorSize: 512, Style: "raw"}
	p := repo.PartitionImage{Number: 1, Offset: 0, Length: size, Included: true, Method: "changed-blocks", Source: source,
		BlockSize: uint32(blockSize), Blocks: (size + blockSize - 1) / blockSize} // #nosec G115 -- validated session setting is at most 1 MiB
	ids := make([]repo.ID, p.Blocks)
	if prev != nil {
		old, err := r.LoadBlockMap(ctx, &prev.Partitions[0])
		if err != nil {
			return img, fmt.Errorf("earlier backup: %w", err)
		}
		copy(ids, old)
		img.Head = prev.Head
	} else {
		p.Method = "used-blocks"
		if reblocked {
			p.Method = "full"
		}
	}
	read := make([]bool, p.Blocks)
	for _, a := range areas {
		if a.Length == 0 || a.Offset >= size {
			continue
		}
		end := min(a.Offset+min(a.Length, size-a.Offset), size)
		for b := a.Offset / blockSize; b*blockSize < end; b++ {
			read[b] = true
		}
	}
	// Keep Head as the first MiB, as in existing repositories, even when
	// the data map uses smaller blocks. Refresh it if any of that region
	// changes, not only when block zero changes.
	headSize := min(uint64(BlockSize), size)
	headBlocks := (headSize + blockSize - 1) / blockSize
	refreshHead := img.Head.IsNull()
	for b := uint64(0); b < headBlocks; b++ {
		refreshHead = refreshHead || read[b]
	}
	if refreshHead {
		for b := uint64(0); b < headBlocks; b++ {
			read[b] = true
		}
	}
	// Blocks are compressed in parallel; they reach the packs in disk order,
	// exactly as with sequential SaveBlob calls.
	bw, err := repo.NewBlobWriter(ctx, r, 0)
	if err != nil {
		return img, err
	}
	defer func() { _ = bw.Close() }()
	runSize := uint64(32) * blockSize
	buf := make([]byte, runSize)
	for i := uint64(0); i < p.Blocks; {
		if !read[i] {
			i++
			continue
		}
		// Read a run of consecutive blocks at once.
		j := i
		for j < p.Blocks && read[j] && (j-i+1)*blockSize <= runSize {
			j++
		}
		start := i * blockSize
		n := min(j*blockSize, size) - start
		if err := ctx.Err(); err != nil {
			return img, err
		}
		if _, err := f.ReadAt(buf[:n], int64(start)); err != nil && !(errors.Is(err, io.EOF) && j == p.Blocks) {
			return img, fmt.Errorf("read at %d: %w", start, err)
		}
		if i == 0 && refreshHead {
			var err error
			if img.Head, _, err = bw.Save(ctx, repo.DataBlob, buf[:headSize]); err != nil {
				return img, err
			}
		}
		for b := i; b < j; b++ {
			blk := buf[(b-i)*blockSize : min((b-i+1)*blockSize, n)]
			if bytes.Equal(blk, zeroBlock[:len(blk)]) {
				ids[b] = repo.ID{}
				continue
			}
			id, _, err := bw.Save(ctx, repo.DataBlob, blk)
			if err != nil {
				return img, err
			}
			ids[b] = id
		}
		progress(n)
		i = j
	}
	if err := bw.Close(); err != nil {
		return img, err
	}
	for i, id := range ids {
		if !id.IsNull() {
			p.StoredBytes += min(blockSize, size-uint64(i)*blockSize)
		}
	}
	p.StoredBytes = min(p.StoredBytes, size)
	if p.Maps, err = r.SaveBlockMap(ctx, ids); err != nil {
		return img, err
	}
	img.Partitions = []repo.PartitionImage{p}
	return img, nil
}

// Previous is the newest backup of a VM disk that recorded a change
// tracking position, for changed-block backups.
type Previous struct {
	Image    *repo.DiskImage
	ChangeID string
}

// PreviousDisks finds, per VM ID and disk key ("<id>/<key>", lower case),
// the newest backup of each disk of the platform's guests in r.
func PreviousDisks(ctx context.Context, r *repo.Repository, platform string) (map[string]Previous, error) {
	snaps, err := r.ListSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]Previous{}
	for i := len(snaps) - 1; i >= 0; i-- { // newest first
		sn := snaps[i]
		for _, g := range sn.Guests {
			if g.Platform != platform {
				continue
			}
			for _, d := range g.Disks {
				k := PreviousKey(g.ID, d.Key)
				if _, seen := out[k]; seen || d.ChangeID == "" || d.Image < 0 || d.Image >= len(sn.Images) {
					continue
				}
				img := sn.Images[d.Image]
				out[k] = Previous{Image: &img, ChangeID: d.ChangeID}
			}
		}
	}
	return out, nil
}

// PreviousKey is the key of PreviousDisks.
func PreviousKey(vmID, diskKey string) string { return strings.ToLower(vmID + "/" + diskKey) }

// WriteChanges updates a disk that holds image prev so that it holds img:
// only blocks whose content differs between the two are written (blocks
// that became empty are zeroed). Without a usable prev it writes every
// block, zeros included.
func WriteChanges(ctx context.Context, r *repo.Repository, img, prev *repo.DiskImage, w io.WriterAt, progress func(uint64)) (written uint64, err error) {
	if len(img.Partitions) != 1 {
		return 0, errors.New("not a whole-disk image")
	}
	if progress == nil {
		progress = func(uint64) {}
	}
	p := &img.Partitions[0]
	ids, err := r.LoadBlockMap(ctx, p)
	if err != nil {
		return 0, err
	}
	var old []repo.ID
	if prev != nil && len(prev.Partitions) == 1 && prev.Partitions[0].BlockSize == p.BlockSize {
		if old, err = r.LoadBlockMap(ctx, &prev.Partitions[0]); err != nil {
			old = nil // e.g. removed by retention: write everything
		}
	}
	for i, id := range ids {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		off := uint64(i) * uint64(p.BlockSize)
		n := min(uint64(p.BlockSize), p.Length-off)
		if i < len(old) && old[i] == id {
			progress(n)
			continue
		}
		var b []byte
		if id.IsNull() {
			b = zeroBlock[:n]
		} else {
			if b, err = r.LoadBlob(ctx, repo.DataBlob, id); err != nil {
				return written, fmt.Errorf("block %d: %w", i, err)
			}
			if uint64(len(b)) != n {
				return written, fmt.Errorf("block %d has %d bytes, expected %d", i, len(b), n)
			}
		}
		if _, err := w.WriteAt(b, int64(off)); err != nil { // #nosec G115 -- disk offsets are below 2^63
			return written, fmt.Errorf("write block %d: %w", i, err)
		}
		written += n
		progress(n)
	}
	return written, nil
}
