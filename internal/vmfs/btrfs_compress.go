package vmfs

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
)

// btrfsDecompress returns the ram bytes an extent holds: zlib (1), LZO (2)
// or zstd (3) data as btrfs writes it. A stream that ends early is padded
// with zeros, as the kernel does.
func btrfsDecompress(comp uint8, src []byte, ram, sector int) ([]byte, error) {
	out := make([]byte, ram)
	var n int
	var err error
	switch comp {
	case 1:
		var zr io.ReadCloser
		if zr, err = zlib.NewReader(bytes.NewReader(src)); err == nil {
			n, err = io.ReadFull(zr, out)
			_ = zr.Close()
		}
	case 2:
		n, err = btrfsLZO(src, out, sector)
	case 3:
		var zr *zstd.Decoder
		// btrfs writes windows of at most 128 KiB.
		if zr, err = zstd.NewReader(bytes.NewReader(src), zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true), zstd.WithDecoderMaxWindow(8<<20)); err == nil {
			n, err = io.ReadFull(zr, out)
			zr.Close()
		}
	default:
		return nil, fmt.Errorf("compression type %d", comp)
	}
	if err == io.ErrUnexpectedEOF || err == io.EOF {
		err = nil
	}
	if err != nil {
		return nil, btrfsErr("compressed extent: %v", err)
	}
	clear(out[n:])
	return out, nil
}

// btrfsLZO decodes btrfs's LZO layout: the total length (4 bytes), then
// segments of a 4-byte length and LZO1X data for at most one sector each;
// a segment header never crosses a sector, so up to 3 zero bytes pad it.
func btrfsLZO(src, out []byte, sector int) (int, error) {
	if len(src) < 4 {
		return 0, errLZO
	}
	total := int(binary.LittleEndian.Uint32(src))
	if total < 4 || total > len(src) {
		return 0, errLZO
	}
	src = src[:total]
	seg := make([]byte, sector)
	pos, n := 4, 0
	for pos < total && n < len(out) {
		if left := sector - pos%sector; left < 4 {
			pos += left
		}
		if pos+4 > total {
			break
		}
		l := int(binary.LittleEndian.Uint32(src[pos:]))
		pos += 4
		if l == 0 || l > total-pos {
			return n, errLZO
		}
		got, err := lzo1xDecompress(src[pos:pos+l], seg)
		if err != nil {
			return n, err
		}
		n += copy(out[n:], seg[:got])
		pos += l
	}
	return n, nil
}

var errLZO = errors.New("damaged LZO data")

// lzo1xDecompress decodes one LZO1X stream into dst and returns how many
// bytes it wrote; it never reads or writes outside src and dst. Written
// from the published description of the format (instructions, match
// distances and the 0x11 0x00 0x00 end marker).
func lzo1xDecompress(src, dst []byte) (int, error) {
	ip, op := 0, 0
	if len(src) >= 5 && src[0] == 17 {
		return 0, errors.New("LZO stream with a version header (lzo-rle), which btrfs does not write")
	}
	literals := func(n int) error {
		if n > len(src)-ip || n > len(dst)-op {
			return errLZO
		}
		copy(dst[op:], src[ip:ip+n])
		ip, op = ip+n, op+n
		return nil
	}
	// count reads a length that continues in the next bytes: each zero
	// byte adds 255, the first non-zero byte ends it.
	count := func(base int) (int, error) {
		for ip < len(src) && src[ip] == 0 {
			ip++
			base += 255
			if base > len(dst) {
				return 0, errLZO
			}
		}
		if ip >= len(src) {
			return 0, errLZO
		}
		base += int(src[ip])
		ip++
		return base, nil
	}
	state := 0 // literals copied by the last instruction (4: four or more)
	if len(src) > 0 && src[0] > 17 {
		t := int(src[0]) - 17
		ip = 1
		if err := literals(t); err != nil {
			return op, err
		}
		state = min(t, 4)
	}
	for {
		if ip >= len(src) {
			return op, errLZO
		}
		t := int(src[ip])
		ip++
		var dist, length, next int
		switch {
		case t < 16 && state == 0: // a run of literals
			n := t
			if n == 0 {
				var err error
				if n, err = count(15); err != nil {
					return op, err
				}
			}
			if err := literals(n + 3); err != nil {
				return op, err
			}
			state = 4
			continue
		case t < 16: // a short match right after literals
			if ip >= len(src) {
				return op, errLZO
			}
			h := int(src[ip])
			ip++
			if state < 4 {
				dist, length = 1+(t>>2)+(h<<2), 2
			} else {
				dist, length = 2049+(t>>2)+(h<<2), 3
			}
			next = t & 3
		case t < 32: // a match 16 to 48 KiB back, or the end of the stream
			n := t & 7
			if n == 0 {
				var err error
				if n, err = count(7); err != nil {
					return op, err
				}
			}
			if ip+2 > len(src) {
				return op, errLZO
			}
			v := int(src[ip]) | int(src[ip+1])<<8
			ip += 2
			dist, length, next = 16384+(t&8)<<11+v>>2, n+2, v&3
			if dist == 16384 {
				if length != 3 {
					return op, errLZO
				}
				return op, nil
			}
		case t < 64: // a match up to 16 KiB back
			n := t & 31
			if n == 0 {
				var err error
				if n, err = count(31); err != nil {
					return op, err
				}
			}
			if ip+2 > len(src) {
				return op, errLZO
			}
			v := int(src[ip]) | int(src[ip+1])<<8
			ip += 2
			dist, length, next = v>>2+1, n+2, v&3
		default: // a match of 3 to 8 bytes up to 2 KiB back
			if ip >= len(src) {
				return op, errLZO
			}
			h := int(src[ip])
			ip++
			dist, length, next = (t>>2)&7+h<<3+1, t>>5+1, t&3
		}
		if dist > op || length > len(dst)-op {
			return op, errLZO
		}
		for i := 0; i < length; i++ { // the source may overlap what is written
			dst[op] = dst[op-dist]
			op++
		}
		if err := literals(next); err != nil {
			return op, err
		}
		state = next
	}
}
