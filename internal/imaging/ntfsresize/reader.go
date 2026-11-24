package ntfsresize

import (
	"errors"
	"io"
	"math"
)

// AlignedReader reads one partition of a disk or image with every read
// widened to whole blocks: raw Windows disk devices refuse reads that do
// not start and end on a sector boundary. Use it to give Open and Verify
// a partition of a disk handle.
type AlignedReader struct {
	r     io.ReaderAt
	base  int64
	size  int64 // 0: unknown
	align int64
}

// NewAlignedReader reads size bytes (0 if unknown) at byte base of r in
// blocks of align bytes (the sector size; 4096 if it is not a power of
// two from 512 up).
func NewAlignedReader(r io.ReaderAt, base, size int64, align int) *AlignedReader {
	a := int64(align)
	if a < 512 || a&(a-1) != 0 {
		a = 4096
	}
	return &AlignedReader{r: r, base: base, size: size, align: a}
}

// Size is the partition length given to NewAlignedReader (0 if unknown).
func (a *AlignedReader) Size() int64 { return a.size }

// ReadAt reads len(p) bytes at off of the partition.
func (a *AlignedReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || a.base < 0 || off > math.MaxInt64-a.base {
		return 0, errors.New("aligned reader: offset out of range")
	}
	var eof error
	if a.size > 0 {
		if off >= a.size {
			return 0, io.EOF
		}
		if rest := a.size - off; int64(len(p)) > rest {
			p, eof = p[:rest], io.EOF
		}
	}
	abs := a.base + off
	if int64(len(p)) > math.MaxInt64-abs-a.align {
		return 0, errors.New("aligned reader: read out of range")
	}
	start := abs / a.align * a.align
	end := (abs + int64(len(p)) + a.align - 1) / a.align * a.align
	buf := make([]byte, end-start)
	n, err := a.r.ReadAt(buf, start)
	skip := abs - start
	if int64(n) <= skip {
		if err == nil {
			err = io.EOF
		}
		return 0, err
	}
	c := copy(p, buf[skip:n])
	if c == len(p) {
		return c, eof
	}
	if err == nil {
		err = io.EOF
	}
	return c, err
}
