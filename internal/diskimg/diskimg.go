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
