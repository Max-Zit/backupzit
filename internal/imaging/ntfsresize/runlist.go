// Package ntfsresize shrinks NTFS file systems: clusters behind the new
// end move to free clusters before it, the file records that point to
// them are rewritten, and the volume metadata ($Bitmap, $BadClus, boot
// sectors) gets the new size. It works on a source image (read only) and
// writes the changes to a target, the way ntfsresize from ntfs-3g does in
// place. Afterwards the volume is marked dirty, so Windows runs chkdsk
// once at the next start.
package ntfsresize

import (
	"errors"
	"fmt"
)

// Run is one extent of a non-resident attribute: Len clusters starting at
// virtual cluster VCN, stored at logical cluster LCN (none if Sparse).
type Run struct {
	VCN    uint64
	LCN    uint64
	Len    uint64
	Sparse bool
}

// decodeRuns parses NTFS mapping pairs starting at virtual cluster vcn.
func decodeRuns(b []byte, vcn uint64) ([]Run, error) {
	var runs []Run
	var lcn int64
	for i := 0; i < len(b); {
		h := b[i]
		if h == 0 {
			return runs, nil
		}
		ls, os := int(h&0x0F), int(h>>4)
		if ls == 0 || ls > 8 || os > 8 || i+1+ls+os > len(b) {
			return nil, errors.New("invalid mapping pairs")
		}
		var length uint64
		for k := 0; k < ls; k++ {
			length |= uint64(b[i+1+k]) << (8 * k)
		}
		r := Run{VCN: vcn, Len: length}
		if os == 0 {
			r.Sparse = true
		} else {
			var d int64
			for k := 0; k < os; k++ {
				d |= int64(b[i+1+ls+k]) << (8 * k)
			}
			// Sign-extend.
			if b[i+ls+os]&0x80 != 0 {
				d |= -1 << (8 * os)
			}
			lcn += d
			if lcn < 0 {
				return nil, errors.New("negative cluster number in mapping pairs")
			}
			r.LCN = uint64(lcn)
		}
		runs = append(runs, r)
		vcn += length
		i += 1 + ls + os
	}
	return nil, errors.New("mapping pairs not terminated")
}

// bytesUnsigned is the number of bytes needed for v as unsigned.
func bytesUnsigned(v uint64) int {
	n := 1
	for v >>= 8; v != 0; v >>= 8 {
		n++
	}
	return n
}

// bytesSigned is the number of bytes needed for v as two's complement.
func bytesSigned(v int64) int {
	for n := 1; n < 8; n++ {
		lim := int64(1) << (8*n - 1)
		if v >= -lim && v < lim {
			return n
		}
	}
	return 8
}

// encodeRuns builds mapping pairs (with the terminating zero) for runs.
func encodeRuns(runs []Run) []byte {
	var out []byte
	var prev int64
	for _, r := range runs {
		ls := bytesUnsigned(r.Len)
		h := byte(ls)
		var off []byte
		if !r.Sparse {
			d := int64(r.LCN) - prev // #nosec G115 -- cluster numbers fit in int64
			os := bytesSigned(d)
			h |= byte(os) << 4
			for k := 0; k < os; k++ {
				off = append(off, byte(d>>(8*k)))
			}
			prev = int64(r.LCN) // #nosec G115 -- cluster numbers fit in int64
		}
		out = append(out, h)
		for k := 0; k < ls; k++ {
			out = append(out, byte(r.Len>>(8*k)))
		}
		out = append(out, off...)
	}
	return append(out, 0)
}

// mergeRuns joins adjacent runs that continue each other.
func mergeRuns(runs []Run) []Run {
	var out []Run
	for _, r := range runs {
		if r.Len == 0 {
			continue
		}
		if n := len(out); n > 0 {
			p := &out[n-1]
			if p.Sparse == r.Sparse && p.VCN+p.Len == r.VCN && (r.Sparse || p.LCN+p.Len == r.LCN) {
				p.Len += r.Len
				continue
			}
		}
		out = append(out, r)
	}
	return out
}

// lcnOf maps a virtual cluster to its logical cluster.
func lcnOf(runs []Run, vcn uint64) (uint64, error) {
	for _, r := range runs {
		if vcn >= r.VCN && vcn < r.VCN+r.Len {
			if r.Sparse {
				return 0, fmt.Errorf("cluster %d is sparse", vcn)
			}
			return r.LCN + vcn - r.VCN, nil
		}
	}
	return 0, fmt.Errorf("cluster %d is not mapped", vcn)
}
