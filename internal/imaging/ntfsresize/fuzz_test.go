package ntfsresize

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"testing"
)

// fuzzVolumes are the synthetic volumes FuzzOpen patches: small (128
// clusters of 512 bytes, $Boot of 1 KiB) with a moved file, a resident
// file, a hibernation file and an extension record, clean or not.
func fuzzVolumes() []*synthVolume {
	files := []synthFile{
		{num: 9, name: "a.bin", runs: []Run{{VCN: 0, LCN: 60, Len: 4}, {VCN: 4, Len: 4, Sparse: true}, {VCN: 8, LCN: 100, Len: 3}}},
		{num: 10, name: "b.txt", head: []byte("hello")},
		{num: 11, name: "hiberfil.sys", runs: []Run{{VCN: 0, LCN: 64, Len: 2}}},
		{num: 12, name: "c.bin", ext: 13, runs: []Run{{VCN: 0, LCN: 110, Len: 2}}},
	}
	return []*synthVolume{
		{boot: 2, files: files},
		{boot: 2, files: files, volFlags: volumeDirty},
		{boot: 2, files: files, logSet: true, logMajor: 2, logInUse: 0},
		{boot: 2, files: files, logEmpty: true},
	}
}

// fuzzBases are the images of fuzzVolumes, built once.
var fuzzBases [][]byte

// FuzzOpen feeds crafted volumes to Open and, when it accepts them, to
// everything that reads a volume: Check, the reports, MinimumSize,
// PlanWith, Apply and Verify. Nothing may panic, and a plan that applied
// must verify. The input picks a base volume and overwrites two regions
// of it, so the fuzzer works on small inputs (whole images of tens of
// KiB stall Go's fuzzer) while any byte can still be crafted; the seeds
// are the metadata regions as they are: boot sector, $MFT records,
// $Bitmap and the $LogFile restart page.
func FuzzOpen(f *testing.F) {
	vols := fuzzVolumes()
	fuzzBases = nil
	for _, s := range vols {
		fuzzBases = append(fuzzBases, s.build(f))
	}
	l, cs, rs := vols[0].layout(), vols[0].cs, vols[0].rs
	img := fuzzBases[0]
	region := func(off, n uint64) []byte { return append([]byte(nil), img[off:off+n]...) }
	for which := range fuzzBases {
		w := uint8(which)
		f.Add(w, uint32(0), region(0, 512), uint32(l.bm*cs), region(l.bm*cs, 16))
		for _, num := range []uint64{recMFT, recLogFile, recVolume, recBitmap, recBadClus, 9, 11, 12, 13} {
			off := l.mft*cs + num*rs
			f.Add(w, uint32(off), region(off, rs), uint32(l.log*cs), region(l.log*cs, 64))
		}
	}
	f.Fuzz(func(t *testing.T, which uint8, off1 uint32, patch1 []byte, off2 uint32, patch2 []byte) {
		base := fuzzBases[int(which)%len(fuzzBases)]
		img := append([]byte(nil), base...)
		for _, p := range []struct {
			off  uint32
			data []byte
		}{{off1, patch1}, {off2, patch2}} {
			if o := int(p.off) % len(img); len(p.data) <= len(img)-o {
				copy(img[o:], p.data)
			}
		}
		v, err := Open(bytes.NewReader(img))
		if err != nil {
			return
		}
		_, _ = v.Check(4)
		_ = v.StateReport()
		_, _ = v.BitmapReport()
		for _, opts := range []PlanOptions{{}, snapshot, {NoMove: true}} {
			_, _ = v.MinimumSize(opts)
			full := v.UnchangedSize()
			for _, size := range []uint64{full - 8*v.ClusterSize, full / 2, full / 4 * 3} {
				p, err := v.PlanWith(size, opts)
				if err != nil {
					continue
				}
				dst := &memDisk{b: append([]byte(nil), img[:min(size, uint64(len(img)))]...)}
				if err := p.Apply(dst, nil); err != nil {
					continue
				}
				if err := p.Verify(bytes.NewReader(dst.b)); err != nil {
					t.Fatalf("size %d, %+v: applied but does not verify: %v", size, opts, err)
				}
			}
		}
	})
}

// FuzzParseRecord feeds crafted file records to the fixup and record
// parser, re-encodes them with changed runs, and checks that the size
// Plan computes for a record matches what encode builds.
func FuzzParseRecord(f *testing.F) {
	for _, s := range fuzzVolumes()[:1] {
		img := s.build(f)
		l := s.layout()
		for num := uint64(0); num < 14; num++ {
			off := l.mft*s.cs + num*s.rs
			f.Add(append([]byte(nil), img[off:off+s.rs]...), false)
		}
	}
	f.Fuzz(func(t *testing.T, data []byte, big bool) {
		size := 1024
		if big {
			size = 4096
		}
		b := make([]byte, size)
		copy(b, data)
		if removeFixups(b) != nil {
			return
		}
		r, err := parseRecord(9, b)
		if err != nil {
			return
		}
		for _, a := range r.Attrs {
			_ = a.residentValue()
			_, _, _ = a.fileName()
		}
		_ = r.hasName(recRoot, "hiberfil.sys")
		c := r.clone()
		changed := map[int][]Run{}
		for i, a := range c.Attrs {
			if !a.NonResident {
				continue
			}
			runs := append([]Run(nil), a.Runs...)
			end := a.StartVCN
			if n := len(runs); n > 0 {
				end = runs[n-1].VCN + runs[n-1].Len
			}
			if end <= maxVCN-3 {
				runs = append(runs, Run{VCN: end, LCN: 12345, Len: 3})
			}
			a.Runs, a.dirty = runs, true
			changed[i] = runs
		}
		enc, err := c.encode()
		if fits := recordLen(r, changed) <= size; fits != (err == nil) {
			t.Fatalf("recordLen says fits=%v, encode: %v", fits, err)
		}
		if err != nil {
			return
		}
		applyFixups(enc)
		if err := removeFixups(enc); err != nil {
			t.Fatalf("own fixups: %v", err)
		}
		back, err := parseRecord(9, enc)
		if err != nil {
			t.Fatalf("re-parse: %v", err)
		}
		for i, runs := range changed {
			if fmt.Sprint(back.Attrs[i].Runs) != fmt.Sprint(runs) {
				t.Fatalf("attribute %d: runs %v, want %v", i, back.Attrs[i].Runs, runs)
			}
		}
	})
}

// noPanic runs fn and turns a panic into a test failure.
func noPanic(t *testing.T, name string, fn func() error) error {
	t.Helper()
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("%s: panic: %v", name, r)
			}
		}()
		err = fn()
	}()
	return err
}

// Should 16: sizes read from disk are bounded before memory is allocated
// for them. A crafted $Bitmap size made Open panic with "makeslice: len
// out of range".
func TestOpenRejectsCraftedSizes(t *testing.T) {
	src := testImage(t)
	type patch func(img []byte)
	sized := func(num uint64, name string, fn func(a *Attribute)) patch {
		return func(img []byte) {
			patchRecord(t, img, num, func(r *Record) {
				a := r.named(attrData, name)
				fn(a)
				a.dirty = true
			})
		}
	}
	for name, p := range map[string]patch{
		"$Bitmap data size": sized(recBitmap, "", func(a *Attribute) { a.DataSize, a.InitializedSize = 1<<62, 1<<62 }),
		"$Bitmap all sizes": sized(recBitmap, "", func(a *Attribute) { a.AllocSize, a.DataSize, a.InitializedSize = 1<<62, 1<<62, 1<<62 }),
		"$Bitmap too small": sized(recBitmap, "", func(a *Attribute) { a.DataSize, a.InitializedSize = 100, 100 }),
		"$MFT sizes":        sized(recMFT, "", func(a *Attribute) { a.InitializedSize = 1 << 40 }),
		"sector count":      func(img []byte) { binary.LittleEndian.PutUint64(img[0x28:], 1<<60) },
		"cluster size":      func(img []byte) { img[0x0D] = 0x81 },
		"record size":       func(img []byte) { img[0x40] = 0x80 },
		"record size 0":     func(img []byte) { img[0x40] = 0 },
		"$MFT position":     func(img []byte) { binary.LittleEndian.PutUint64(img[0x30:], 1<<40) },
	} {
		img := append([]byte(nil), src...)
		p(img)
		err := noPanic(t, name, func() error { _, err := Open(bytes.NewReader(img)); return err })
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else {
			t.Logf("%s: %v", name, err)
		}
	}
}

// The bitmap helpers against bit-by-bit versions.
func TestBitHelpers(t *testing.T) {
	rnd := rand.New(rand.NewSource(7))
	bit := func(b []byte, c uint64) bool { return b[c/8]&(1<<(c%8)) != 0 }
	for iter := 0; iter < 2000; iter++ {
		n := 1 + rnd.Intn(12)
		a, b := make([]byte, n), make([]byte, n)
		for i := range a {
			a[i], b[i] = byte(rnd.Intn(256)), byte(rnd.Intn(256))
			if rnd.Intn(3) == 0 {
				a[i] = 0xFF * byte(rnd.Intn(2))
			}
		}
		total := uint64(n * 8)
		s := uint64(rnd.Int63n(int64(total)))
		e := s + uint64(rnd.Int63n(int64(total-s)+1))
		var ones, andNot uint64
		firstSetWant, firstClearWant := e, e
		for c := s; c < e; c++ {
			if bit(a, c) {
				ones++
				firstSetWant = min(firstSetWant, c)
			} else {
				firstClearWant = min(firstClearWant, c)
			}
			if bit(a, c) && !bit(b, c) {
				andNot++
			}
		}
		if got := countOnes(a, s, e); got != ones {
			t.Fatalf("countOnes %x [%d,%d) = %d, want %d", a, s, e, got, ones)
		}
		if got := countAndNot(a, b, s, e); got != andNot {
			t.Fatalf("countAndNot = %d, want %d", got, andNot)
		}
		if c, ok := firstSet(a, s, e-s); ok != (firstSetWant < e) || (ok && c != firstSetWant) {
			t.Fatalf("firstSet %x [%d,%d) = %d %v, want %d", a, s, e, c, ok, firstSetWant)
		}
		if c, ok := firstClear(a, s, e-s); ok != (firstClearWant < e) || (ok && c != firstClearWant) {
			t.Fatalf("firstClear %x [%d,%d) = %d %v, want %d", a, s, e, c, ok, firstClearWant)
		}
		lastWant, any := uint64(0), false
		for c := uint64(0); c < e; c++ {
			if bit(a, c) {
				lastWant, any = c, true
			}
		}
		if c, ok := lastSet(a, e); ok != any || c != lastWant {
			t.Fatalf("lastSet %x below %d = %d %v, want %d %v", a, e, c, ok, lastWant, any)
		}
		var free []extent
		forEachFree(a, e, func(st, l uint64) { free = append(free, extent{st, l}) })
		var wantFree []extent
		for c := uint64(0); c < e; c++ {
			if !bit(a, c) {
				if k := len(wantFree); k > 0 && wantFree[k-1].start+wantFree[k-1].len == c {
					wantFree[k-1].len++
				} else {
					wantFree = append(wantFree, extent{c, 1})
				}
			}
		}
		if fmt.Sprint(free) != fmt.Sprint(wantFree) {
			t.Fatalf("forEachFree %x below %d = %v, want %v", a, e, free, wantFree)
		}
		on := rnd.Intn(2) == 0
		c := append([]byte(nil), a...)
		setBits(c, s, e-s, on)
		for k := uint64(0); k < total; k++ {
			want := bit(a, k)
			if k >= s && k < e {
				want = on
			}
			if bit(c, k) != want {
				t.Fatalf("setBits %x [%d,%d) %v: bit %d", a, s, e, on, k)
			}
		}
	}
}
