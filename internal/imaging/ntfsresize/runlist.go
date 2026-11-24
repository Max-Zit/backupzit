// Package ntfsresize shrinks NTFS file systems: clusters behind the new
// end move to free clusters before it, the file records that point to
// them are rewritten, and the volume metadata ($Bitmap, $BadClus, boot
// sectors) gets the new size. It works on a source image (read only) and
// writes the changes to a target, the way ntfsresize from ntfs-3g does in
// place. Afterwards the volume is marked dirty, so Windows runs chkdsk
// once at the next start.
//
// Nothing is planned on a source that is not consistent: Plan checks
// every file record against $Bitmap, and refuses volumes that are marked
// dirty, were not shut down cleanly or hold an active hibernation image,
// before anything is written.
package ntfsresize

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
)

// maxVCN is the largest cluster number (virtual or logical) the codec
// accepts. NTFS stores cluster numbers as signed 64-bit values, so this
// keeps every number and every run end representable.
const maxVCN = math.MaxInt64

// Run is one extent of a non-resident attribute: Len clusters starting at
// virtual cluster VCN, stored at logical cluster LCN (none if Sparse).
type Run struct {
	VCN    uint64
	LCN    uint64
	Len    uint64
	Sparse bool
}

// decodeRuns parses NTFS mapping pairs starting at virtual cluster vcn.
// Every run it returns ends at or below maxVCN, so VCN+Len and LCN+Len
// cannot overflow.
func decodeRuns(b []byte, vcn uint64) ([]Run, error) {
	if vcn > maxVCN {
		return nil, errors.New("invalid starting cluster in mapping pairs")
	}
	var runs []Run
	var lcn uint64
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
		// Lengths are signed like the offsets: Windows rejects a run whose
		// stored length has the top bit set.
		if b[i+ls]&0x80 != 0 || length == 0 {
			return nil, errors.New("invalid (negative or zero) run length in mapping pairs")
		}
		if length > maxVCN-vcn {
			return nil, errors.New("mapping pairs run past the largest cluster number")
		}
		r := Run{VCN: vcn, Len: length}
		if os == 0 {
			r.Sparse = true
		} else {
			// The offset is a signed delta to the previous run's LCN. It is
			// handled as the bits of a two's complement number, so no
			// signed conversion is needed.
			var d uint64
			for k := 0; k < os; k++ {
				d |= uint64(b[i+1+ls+k]) << (8 * k)
			}
			if b[i+ls+os]&0x80 != 0 {
				if os < 8 {
					d |= ^uint64(0) << (8 * os) // sign-extend
				}
				back := -d // the magnitude of the negative delta
				if back > lcn {
					return nil, errors.New("negative cluster number in mapping pairs")
				}
				lcn -= back
			} else {
				if d > maxVCN-lcn {
					return nil, errors.New("cluster number in mapping pairs too large")
				}
				lcn += d
			}
			if length > maxVCN-lcn {
				return nil, errors.New("mapping pairs run past the largest cluster number")
			}
			r.LCN = lcn
		}
		runs = append(runs, r)
		vcn += length
		i += 1 + ls + os
	}
	return nil, errors.New("mapping pairs not terminated")
}

// signedBytes is the number of bytes NTFS needs to store v, read as a two's
// complement number, without changing its value or sign: bits 8n-1 to 63
// must all be equal.
func signedBytes(v uint64) byte {
	for n := byte(1); n < 8; n++ {
		shift := 8*uint(n) - 1
		if top := v >> shift; top == 0 || top == ^uint64(0)>>shift {
			return n
		}
	}
	return 8
}

// pairsLen is the length of encodeRuns(runs), computed without building it.
// Plan uses it to check that rewritten records still fit.
func pairsLen(runs []Run) int {
	n := 1 // terminating zero
	var prev uint64
	for _, r := range runs {
		n += 1 + int(signedBytes(r.Len))
		if !r.Sparse {
			n += int(signedBytes(r.LCN - prev))
			prev = r.LCN
		}
	}
	return n
}

// encodeRuns builds mapping pairs (with the terminating zero) for runs.
// Run lengths and LCNs must be below 2^63, which decodeRuns and the
// volume bounds guarantee.
func encodeRuns(runs []Run) []byte {
	out := make([]byte, 0, pairsLen(runs))
	var prev uint64
	var tmp [8]byte
	for _, r := range runs {
		// Signed, as NTFS reads it: 0x8982 takes three bytes (82 89 00).
		ls := signedBytes(r.Len)
		head := len(out)
		out = append(out, ls)
		binary.LittleEndian.PutUint64(tmp[:], r.Len)
		out = append(out, tmp[:ls]...)
		if !r.Sparse {
			// The wrapping difference is the two's complement of the
			// signed delta.
			d := r.LCN - prev
			os := signedBytes(d)
			out[head] |= os << 4
			binary.LittleEndian.PutUint64(tmp[:], d)
			out = append(out, tmp[:os]...)
			prev = r.LCN
		}
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

// findRun returns the run that maps virtual cluster vcn. runs must be
// sorted by VCN and must not overlap, as decodeRuns returns them.
func findRun(runs []Run, vcn uint64) (Run, bool) {
	i := sort.Search(len(runs), func(i int) bool { return runs[i].VCN+runs[i].Len > vcn })
	if i < len(runs) && runs[i].VCN <= vcn {
		return runs[i], true
	}
	return Run{}, false
}

// lcnOf maps a virtual cluster to its logical cluster.
func lcnOf(runs []Run, vcn uint64) (uint64, error) {
	r, ok := findRun(runs, vcn)
	if !ok {
		return 0, fmt.Errorf("cluster %d is not mapped", vcn)
	}
	if r.Sparse {
		return 0, fmt.Errorf("cluster %d is sparse", vcn)
	}
	return r.LCN + vcn - r.VCN, nil
}

// inVolume tells whether a non-sparse run lies inside a volume of
// clusters clusters, in a way that cannot overflow.
func inVolume(r Run, clusters uint64) bool {
	return r.LCN <= clusters && r.Len <= clusters-r.LCN
}

// runClusters sums the lengths of runs; ok is false when the sum would
// exceed maxVCN (only crafted run lists do that).
func runClusters(runs []Run) (n uint64, ok bool) {
	for _, r := range runs {
		if r.Len > maxVCN-n {
			return 0, false
		}
		n += r.Len
	}
	return n, true
}
