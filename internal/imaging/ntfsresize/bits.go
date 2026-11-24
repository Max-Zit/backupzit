package ntfsresize

import "math/bits"

// Cluster bitmaps: bit c%8 of byte c/8 stands for cluster c, as in
// $Bitmap. These helpers work on ranges a byte at a time where they can,
// since volumes have hundreds of millions of clusters. Callers keep the
// ranges inside the bitmap.

// onesCount64 is bits.OnesCount64 as a uint64.
func onesCount64(x uint64) uint64 {
	return uint64(bits.OnesCount64(x)) // #nosec G115 -- OnesCount64 returns 0..64
}

// countOnes counts the set bits of bm in [start, end).
func countOnes(bm []byte, start, end uint64) uint64 {
	var n uint64
	c := start
	for ; c < end && c%8 != 0; c++ {
		n += uint64(bm[c/8] >> (c % 8) & 1)
	}
	for ; c+8 <= end; c += 8 {
		n += onesCount64(uint64(bm[c/8]))
	}
	for ; c < end; c++ {
		n += uint64(bm[c/8] >> (c % 8) & 1)
	}
	return n
}

// countAndNot counts the bits in [start, end) that are set in a and clear
// in b: allocated clusters that no file owns.
func countAndNot(a, b []byte, start, end uint64) uint64 {
	var n uint64
	c := start
	for ; c < end && c%8 != 0; c++ {
		n += uint64((a[c/8] &^ b[c/8]) >> (c % 8) & 1)
	}
	for ; c+8 <= end; c += 8 {
		n += onesCount64(uint64(a[c/8] &^ b[c/8]))
	}
	for ; c < end; c++ {
		n += uint64((a[c/8] &^ b[c/8]) >> (c % 8) & 1)
	}
	return n
}

// findBit returns the first bit in [start, start+n) whose value is want.
func findBit(bm []byte, start, n uint64, want bool) (uint64, bool) {
	end := start + n
	skip := byte(0x00) // whole bytes that cannot hold the bit
	if !want {
		skip = 0xFF
	}
	for c := start; c < end; {
		if c%8 == 0 && c+8 <= end && bm[c/8] == skip {
			c += 8
			continue
		}
		if (bm[c/8]&(1<<(c%8)) != 0) == want {
			return c, true
		}
		c++
	}
	return 0, false
}

// firstClear returns the first clear bit in [start, start+n).
func firstClear(bm []byte, start, n uint64) (uint64, bool) { return findBit(bm, start, n, false) }

// firstSet returns the first set bit in [start, start+n).
func firstSet(bm []byte, start, n uint64) (uint64, bool) { return findBit(bm, start, n, true) }

// setBits sets (on) or clears the bits [start, start+n).
func setBits(bm []byte, start, n uint64, on bool) {
	end := start + n
	c := start
	for ; c < end && c%8 != 0; c++ {
		setBit(bm, c, on)
	}
	fill := byte(0x00)
	if on {
		fill = 0xFF
	}
	for ; c+8 <= end; c += 8 {
		bm[c/8] = fill
	}
	for ; c < end; c++ {
		setBit(bm, c, on)
	}
}

// setBit sets or clears one bit.
func setBit(bm []byte, c uint64, on bool) {
	if on {
		bm[c/8] |= 1 << (c % 8)
	} else {
		bm[c/8] &^= 1 << (c % 8)
	}
}

// lastSet returns the highest set bit below end.
func lastSet(bm []byte, end uint64) (uint64, bool) {
	c := end
	for c > 0 {
		if c%8 == 0 && bm[c/8-1] == 0 {
			c -= 8
			continue
		}
		c--
		if bm[c/8]&(1<<(c%8)) != 0 {
			return c, true
		}
	}
	return 0, false
}

// forEachFree calls fn for every run of clear bits below end, in order.
func forEachFree(bm []byte, end uint64, fn func(start, n uint64)) {
	var runStart uint64
	in := false
	for c := uint64(0); c < end; {
		if c%8 == 0 && c+8 <= end {
			switch bm[c/8] {
			case 0xFF:
				if in {
					fn(runStart, c-runStart)
					in = false
				}
				c += 8
				continue
			case 0x00:
				if !in {
					runStart, in = c, true
				}
				c += 8
				continue
			}
		}
		if bm[c/8]&(1<<(c%8)) != 0 {
			if in {
				fn(runStart, c-runStart)
				in = false
			}
		} else if !in {
			runStart, in = c, true
		}
		c++
	}
	if in {
		fn(runStart, end-runStart)
	}
}
