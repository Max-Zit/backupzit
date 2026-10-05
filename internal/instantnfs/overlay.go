// Package instantnfs serves VMs from backups to VMware ESXi over NFSv3:
// instant recovery. Each VM is a directory with its configuration and its
// disks; the disks are read from the backup and their changes kept in
// local sparse files, so the backup itself is never modified.
package instantnfs

import (
	"io"
	"os"
	"sync"
)

// blockSize is the unit of copy-on-write.
const blockSize = 64 << 10

// Overlay is a disk read from a backup with its changes in a local file.
// Written blocks are recorded in a bitmap file, so changes survive a
// restart of the server.
type Overlay struct {
	base io.ReaderAt
	size int64
	data *os.File // sparse, same size as the disk
	bm   *os.File // one bit per block
	mu   sync.RWMutex
	bits []byte
}

// OpenOverlay opens (or creates) the change files of a disk.
func OpenOverlay(base io.ReaderAt, size int64, dataPath, mapPath string) (*Overlay, error) {
	data, err := os.OpenFile(dataPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if fi, _ := data.Stat(); fi.Size() != size {
		if err := data.Truncate(size); err != nil {
			data.Close()
			return nil, err
		}
	}
	bm, err := os.OpenFile(mapPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		data.Close()
		return nil, err
	}
	n := (size/blockSize + 8) / 8
	bits := make([]byte, n)
	if _, err := bm.ReadAt(bits, 0); err != nil && err != io.EOF {
		data.Close()
		bm.Close()
		return nil, err
	}
	return &Overlay{base: base, size: size, data: data, bm: bm, bits: bits}, nil
}

// Size is the size of the disk.
func (o *Overlay) Size() int64 { return o.size }

func (o *Overlay) written(b int64) bool { return o.bits[b/8]&(1<<(b%8)) != 0 }

// ReadAt reads changed blocks from the change file, others from the backup.
func (o *Overlay) ReadAt(p []byte, off int64) (int, error) {
	if off >= o.size {
		return 0, io.EOF
	}
	var eof error
	if off+int64(len(p)) > o.size {
		p, eof = p[:o.size-off], io.EOF
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	done := 0
	for done < len(p) {
		pos := off + int64(done)
		b := pos / blockSize
		// A run of blocks from the same source.
		w := o.written(b)
		end := (b + 1) * blockSize
		for end < off+int64(len(p)) && o.written(end/blockSize) == w {
			end += blockSize
		}
		n := int(min(end, off+int64(len(p))) - pos)
		src := o.base
		if w {
			src = o.data
		}
		if _, err := src.ReadAt(p[done:done+n], pos); err != nil && err != io.EOF {
			return done, err
		}
		done += n
	}
	return done, eof
}

// WriteAt writes into the change file; a block written for the first time
// is first copied from the backup.
func (o *Overlay) WriteAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > o.size {
		return 0, io.ErrShortWrite
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	buf := make([]byte, blockSize)
	for b := off / blockSize; b*blockSize < off+int64(len(p)); b++ {
		if o.written(b) {
			continue
		}
		start := b * blockSize
		covered := start >= off && start+blockSize <= off+int64(len(p))
		if !covered {
			n := int(min(blockSize, o.size-start))
			if _, err := o.base.ReadAt(buf[:n], start); err != nil && err != io.EOF {
				return 0, err
			}
			if _, err := o.data.WriteAt(buf[:n], start); err != nil {
				return 0, err
			}
		}
	}
	if _, err := o.data.WriteAt(p, off); err != nil {
		return 0, err
	}
	// Record the blocks after their data is in place.
	first, last := off/blockSize/8, (off+int64(len(p))-1)/blockSize/8
	for b := off / blockSize; b*blockSize < off+int64(len(p)); b++ {
		o.bits[b/8] |= 1 << (b % 8)
	}
	if _, err := o.bm.WriteAt(o.bits[first:last+1], first); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Sync flushes the change files.
func (o *Overlay) Sync() error {
	if err := o.data.Sync(); err != nil {
		return err
	}
	return o.bm.Sync()
}

// Close closes the change files.
func (o *Overlay) Close() error {
	o.data.Close()
	return o.bm.Close()
}
