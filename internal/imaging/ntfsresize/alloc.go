package ntfsresize

import (
	"errors"
	"math/bits"
)

// maxFreeExtents bounds the free extents the allocator keeps (64 MiB for
// the extents and as much for the tree). Real volumes have far fewer; a
// crafted bitmap with every other cluster free would otherwise need
// gigabytes. Beyond it the shortest extents are left out, which only
// costs free space (planning then asks for a larger size).
const maxFreeExtents = 1 << 22

// extent is a range of free clusters.
type extent struct{ start, len uint64 }

// freeExtents lists the free cluster ranges of bm below end in address
// order, leaving out the shortest ones when there are more than limit
// (maxFreeExtents when planning). The result depends only on its
// arguments, so planning the same size twice gives the same layout.
func freeExtents(bm []byte, end, limit uint64) []extent {
	// Count the extents by bit length of their size (bucket b holds sizes
	// in [2^(b-1), 2^b)), then keep the longest buckets that fit.
	var hist [65]uint64
	forEachFree(bm, end, func(_, n uint64) { hist[bits.Len64(n)]++ })
	minLen, total := uint64(1), uint64(0)
	for b := 63; b >= 1; b-- {
		if total+hist[b] > limit {
			minLen = 1 << b
			break
		}
		total += hist[b]
	}
	out := make([]extent, 0, total)
	forEachFree(bm, end, func(s, n uint64) {
		if n >= minLen {
			out = append(out, extent{s, n})
		}
	})
	return out
}

// errNoSpace is returned when the free space is used up.
var errNoSpace = errors.New("not enough free space before the new end of the volume")

// allocator hands out free clusters below the new end. A segment tree over
// the extents (in address order) keeps the largest extent of each subtree,
// so both queries it needs take O(log n): the lowest extent that holds a
// request, and the largest extent.
type allocator struct {
	ext  []extent
	tree []uint64 // tree[1] is the root; leaf size+i holds ext[i].len
	size int
}

// newAllocator builds an allocator over ext (in address order).
func newAllocator(ext []extent) *allocator {
	size := 1
	for size < len(ext) {
		size <<= 1
	}
	t := make([]uint64, 2*size)
	for i, e := range ext {
		t[size+i] = e.len
	}
	for i := size - 1; i >= 1; i-- {
		t[i] = max(t[2*i], t[2*i+1])
	}
	return &allocator{ext: ext, tree: t, size: size}
}

// fix updates the tree after extent i changed.
func (a *allocator) fix(i int) {
	j := a.size + i
	a.tree[j] = a.ext[i].len
	for j >>= 1; j >= 1; j >>= 1 {
		a.tree[j] = max(a.tree[2*j], a.tree[2*j+1])
	}
}

// firstFit returns the lowest extent with at least n clusters.
func (a *allocator) firstFit(n uint64) (int, bool) {
	if n == 0 || a.tree[1] < n {
		return 0, false
	}
	j := 1
	for j < a.size {
		if a.tree[2*j] >= n {
			j = 2 * j
		} else {
			j = 2*j + 1
		}
	}
	return j - a.size, true
}

// cut takes n clusters from the start of extent i.
func (a *allocator) cut(i int, n uint64) extent {
	e := extent{a.ext[i].start, n}
	a.ext[i].start += n
	a.ext[i].len -= n
	a.fix(i)
	return e
}

// take hands out n clusters: one extent when one is large enough (the
// lowest such), otherwise the largest extents in turn, so a moved stream
// gets as few pieces as possible and its record stays small.
func (a *allocator) take(n uint64) ([]extent, error) {
	if i, ok := a.firstFit(n); ok {
		return []extent{a.cut(i, n)}, nil
	}
	var out []extent
	for n > 0 {
		i, ok := a.firstFit(a.tree[1]) // the largest extent
		if !ok {
			return nil, errNoSpace
		}
		k := min(n, a.ext[i].len)
		out = append(out, a.cut(i, k))
		n -= k
	}
	return out, nil
}
