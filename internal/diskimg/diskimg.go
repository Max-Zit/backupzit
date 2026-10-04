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

	"github.com/backupzit/backupzit/internal/repo"
)

// BlockSize is the unit of deduplication for virtual disks.
const BlockSize = 1 << 20

var zeroBlock = make([]byte, BlockSize)

// Store reads a disk of the given size and saves it as a DiskImage with one
// whole-disk partition. All-zero blocks are recorded as null IDs.
func Store(ctx context.Context, r *repo.Repository, f io.ReaderAt, size uint64, number int, model, source string, progress func(uint64)) (repo.DiskImage, error) {
	if size == 0 {
		return repo.DiskImage{}, errors.New("disk is empty")
	}
	if progress == nil {
		progress = func(uint64) {}
	}
	img := repo.DiskImage{Number: number, Model: model, Size: size, SectorSize: 512, Style: "raw"}
	p := repo.PartitionImage{Number: 1, Offset: 0, Length: size, Included: true, Method: "full", Source: source,
		BlockSize: BlockSize, Blocks: (size + BlockSize - 1) / BlockSize}
	ids := make([]repo.ID, p.Blocks)
	buf := make([]byte, BlockSize)
	for i := uint64(0); i < p.Blocks; i++ {
		if err := ctx.Err(); err != nil {
			return img, err
		}
		n := min(uint64(BlockSize), size-i*BlockSize)
		if _, err := f.ReadAt(buf[:n], int64(i*BlockSize)); err != nil && !(errors.Is(err, io.EOF) && i == p.Blocks-1) {
			return img, fmt.Errorf("read block %d: %w", i, err)
		}
		if i == 0 {
			var err error
			if img.Head, _, err = r.SaveBlob(ctx, repo.DataBlob, buf[:n]); err != nil {
				return img, err
			}
		}
		if bytes.Equal(buf[:n], zeroBlock[:n]) {
			progress(n)
			continue
		}
		id, _, err := r.SaveBlob(ctx, repo.DataBlob, buf[:n])
		if err != nil {
			return img, err
		}
		ids[i] = id
		p.StoredBytes += n
		progress(n)
	}
	var err error
	if p.Maps, err = r.SaveBlockMap(ctx, ids); err != nil {
		return img, err
	}
	img.Partitions = []repo.PartitionImage{p}
	return img, nil
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
				if _, err := w.WriteAt(zeroBlock[:n], int64(off)); err != nil {
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
		if _, err := w.WriteAt(b, int64(off)); err != nil {
			return fmt.Errorf("write block %d: %w", i, err)
		}
		progress(n)
	}
	return nil
}

// Area is a byte range of a disk.
type Area struct{ Offset, Length uint64 }

// maxRun limits how much is read with one ReadAt call.
const maxRun = 32 * BlockSize

// StoreAreas saves a disk like Store but reads only the blocks that overlap
// areas (the allocated or changed regions reported by changed block
// tracking). Blocks outside the areas are taken from prev, the image of the
// same disk in an earlier backup, or are empty when prev is nil.
func StoreAreas(ctx context.Context, r *repo.Repository, f io.ReaderAt, size uint64, areas []Area, prev *repo.DiskImage,
	number int, model, source string, progress func(uint64)) (repo.DiskImage, error) {
	if size == 0 {
		return repo.DiskImage{}, errors.New("disk is empty")
	}
	if progress == nil {
		progress = func(uint64) {}
	}
	img := repo.DiskImage{Number: number, Model: model, Size: size, SectorSize: 512, Style: "raw"}
	p := repo.PartitionImage{Number: 1, Offset: 0, Length: size, Included: true, Method: "changed-blocks", Source: source,
		BlockSize: BlockSize, Blocks: (size + BlockSize - 1) / BlockSize}
	ids := make([]repo.ID, p.Blocks)
	if prev != nil {
		if prev.Size != size || len(prev.Partitions) != 1 || prev.Partitions[0].BlockSize != BlockSize {
			return img, errors.New("the earlier backup of this disk has a different size")
		}
		old, err := r.LoadBlockMap(ctx, &prev.Partitions[0])
		if err != nil {
			return img, fmt.Errorf("earlier backup: %w", err)
		}
		copy(ids, old)
		img.Head = prev.Head
	} else {
		p.Method = "used-blocks"
	}
	read := make([]bool, p.Blocks)
	for _, a := range areas {
		if a.Length == 0 || a.Offset >= size {
			continue
		}
		end := min(a.Offset+a.Length, size)
		for b := a.Offset / BlockSize; b*BlockSize < end; b++ {
			read[b] = true
		}
	}
	if img.Head.IsNull() {
		read[0] = true
	}
	buf := make([]byte, maxRun)
	for i := uint64(0); i < p.Blocks; {
		if !read[i] {
			i++
			continue
		}
		// Read a run of consecutive blocks at once.
		j := i
		for j < p.Blocks && read[j] && (j-i+1)*BlockSize <= maxRun {
			j++
		}
		start := i * BlockSize
		n := min(j*BlockSize, size) - start
		if err := ctx.Err(); err != nil {
			return img, err
		}
		if _, err := f.ReadAt(buf[:n], int64(start)); err != nil && !(errors.Is(err, io.EOF) && j == p.Blocks) {
			return img, fmt.Errorf("read at %d: %w", start, err)
		}
		for b := i; b < j; b++ {
			blk := buf[(b-i)*BlockSize : min((b-i+1)*BlockSize, n)]
			if b == 0 {
				var err error
				if img.Head, _, err = r.SaveBlob(ctx, repo.DataBlob, blk); err != nil {
					return img, err
				}
			}
			if bytes.Equal(blk, zeroBlock[:len(blk)]) {
				ids[b] = repo.ID{}
				continue
			}
			id, _, err := r.SaveBlob(ctx, repo.DataBlob, blk)
			if err != nil {
				return img, err
			}
			ids[b] = id
		}
		progress(n)
		i = j
	}
	for _, id := range ids {
		if !id.IsNull() {
			p.StoredBytes += BlockSize
		}
	}
	p.StoredBytes = min(p.StoredBytes, size)
	var err error
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
