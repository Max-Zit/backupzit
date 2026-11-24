package ntfsresize

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"
)

func TestRunlistRoundTrip(t *testing.T) {
	cases := [][]Run{
		{{VCN: 0, LCN: 3413, Len: 64}},
		{{VCN: 0, LCN: 100, Len: 10}, {VCN: 10, Len: 5, Sparse: true}, {VCN: 15, LCN: 50, Len: 300}, {VCN: 315, LCN: 1 << 33, Len: 1}},
		{{VCN: 0, Len: 1 << 40, Sparse: true}},
	}
	for _, runs := range cases {
		enc := encodeRuns(runs)
		if len(enc) != pairsLen(runs) {
			t.Errorf("pairsLen %d, encoded %d bytes", pairsLen(runs), len(enc))
		}
		got, err := decodeRuns(enc, 0)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(got) != fmt.Sprint(runs) {
			t.Errorf("round trip %v -> %v", runs, got)
		}
	}
	if _, err := decodeRuns([]byte{0x11, 5}, 0); err == nil {
		t.Error("unterminated mapping pairs accepted")
	}
}

// NTFS reads run lengths as signed numbers: a length whose top stored
// byte has bit 7 set is negative, so 0x8982 needs three bytes (82 89 00).
// Found on a real Windows volume (chkdsk: "extent list could not be
// initialized").
func TestRunLengthIsSigned(t *testing.T) {
	enc := encodeRuns([]Run{{VCN: 0, LCN: 3475731, Len: 0x8982}})
	ls := int(enc[0] & 0x0F)
	if top := enc[ls]; top&0x80 != 0 {
		t.Errorf("length 0x8982 stored as % x: top byte %#x reads as negative", enc[1:1+ls], top)
	}
	if _, err := decodeRuns([]byte{0x32, 0x82, 0x89, 0x13, 0x09, 0x35, 0x00}, 0); err == nil {
		t.Error("a negative run length was accepted")
	}
	if runs, err := decodeRuns([]byte{0x33, 0x82, 0x89, 0x00, 0x7c, 0x66, 0x3f, 0x00}, 0); err != nil || runs[0].Len != 0x8982 || runs[0].LCN != 4155004 {
		t.Errorf("Windows' encoding: %v, %v", runs, err)
	}
}

// N8: run ends cannot overflow, and LCNs cannot go negative.
func TestDecodeRunsBoundsClusterNumbers(t *testing.T) {
	if _, err := decodeRuns([]byte{0x01, 0x10, 0x00}, maxVCN-5); err == nil {
		t.Error("a run past the largest VCN accepted")
	}
	if _, err := decodeRuns([]byte{0x01, 0x01, 0x00}, maxVCN+1); err == nil {
		t.Error("a starting VCN past the largest accepted")
	}
	if _, err := decodeRuns([]byte{0x81, 0x10, 0xf0, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f, 0x00}, 0); err == nil {
		t.Error("a run past the largest LCN accepted")
	}
	if _, err := decodeRuns([]byte{0x11, 0x01, 0x80, 0x00}, 0); err == nil {
		t.Error("a negative LCN accepted")
	}
}

// FuzzDecodeRuns checks the mapping pairs decoder on crafted input: what
// it accepts must round-trip through encodeRuns, whose output is never
// longer than the input and as long as pairsLen says.
func FuzzDecodeRuns(f *testing.F) {
	f.Add([]byte{0x31, 0x40, 0x55, 0x0D, 0x00, 0x00}, uint64(0))
	f.Add([]byte{0x11, 0x0a, 0x64, 0x01, 0x05, 0x21, 0x2c, 0x01, 0xce, 0x00}, uint64(10))
	f.Add([]byte{0x33, 0x82, 0x89, 0x00, 0x7c, 0x66, 0x3f, 0x00}, uint64(0))
	f.Add([]byte{0x48, 0xff, 0xff, 0xff, 0x7f, 1, 2, 3, 4, 5, 6, 7, 8, 0x00}, uint64(0))
	f.Add([]byte{0x88, 1, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f, 0x00}, uint64(maxVCN-1))
	f.Fuzz(func(t *testing.T, b []byte, vcn uint64) {
		runs, err := decodeRuns(b, vcn)
		if err != nil {
			return
		}
		next := vcn
		for _, r := range runs {
			if r.VCN != next || r.Len == 0 || r.Len > maxVCN-r.VCN || (!r.Sparse && r.Len > maxVCN-r.LCN) {
				t.Fatalf("run %+v out of range", r)
			}
			next += r.Len
		}
		enc := encodeRuns(runs)
		if len(enc) != pairsLen(runs) {
			t.Fatalf("pairsLen %d, encoded %d", pairsLen(runs), len(enc))
		}
		back, err := decodeRuns(enc, vcn)
		if err != nil || fmt.Sprint(back) != fmt.Sprint(runs) {
			t.Fatalf("round trip %v -> %x -> %v, %v", runs, enc, back, err)
		}
		if len(enc) > len(b)+1 {
			t.Fatalf("encoding grew: %x -> %x", b, enc)
		}
	})
}

func TestFixupsRoundTrip(t *testing.T) {
	b := make([]byte, 1024)
	copy(b, "FILE")
	binary.LittleEndian.PutUint16(b[4:], 0x30)
	binary.LittleEndian.PutUint16(b[6:], 3)
	b[510], b[511], b[1022], b[1023] = 0xAA, 0xBB, 0xCC, 0xDD
	orig := append([]byte(nil), b...)
	applyFixups(b)
	if b[510] == 0xAA {
		t.Fatal("fixup not applied")
	}
	if err := removeFixups(b); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b[0x36:], orig[0x36:]) || !bytes.Equal(b[:0x30], orig[:0x30]) {
		t.Error("fixups did not round-trip")
	}
	b[1022] ^= 1
	applyFixups(b)
	b[1022] ^= 0xFF
	if err := removeFixups(b); err == nil {
		t.Error("torn record accepted")
	}
}

// 4 KiB records (4Kn disks): the stride stays 512 bytes, so there are nine
// entries; a torn last sector is found, and the count must match the size.
func TestFixups4K(t *testing.T) {
	b := make([]byte, 4096)
	copy(b, "FILE")
	binary.LittleEndian.PutUint16(b[4:], 0x30)
	binary.LittleEndian.PutUint16(b[6:], 9)
	for i := 0x30 + 18; i < len(b); i++ {
		b[i] = byte(i * 7)
	}
	orig := append([]byte(nil), b...)
	applyFixups(b)
	for i := 1; i < 9; i++ {
		if !bytes.Equal(b[i*512-2:i*512], b[0x30:0x32]) {
			t.Errorf("sector %d: update sequence number not placed", i)
		}
	}
	if err := removeFixups(b); err != nil || !bytes.Equal(b[0x30+18:], orig[0x30+18:]) {
		t.Fatalf("round trip: %v", err)
	}
	applyFixups(b)
	b[4095] ^= 0xFF
	if removeFixups(b) == nil {
		t.Error("torn last sector accepted")
	}
	binary.LittleEndian.PutUint16(b[6:], 3)
	if removeFixups(b) == nil {
		t.Error("an update sequence covering only part of the record accepted")
	}
}

// testNonResident builds a non-resident $DATA attribute named name.
func testNonResident(name string, runs []Run) []byte {
	pairs := encodeRuns(runs)
	mp := (0x40 + 2*len(name) + 7) &^ 7
	b := make([]byte, (mp+len(pairs)+7)&^7)
	binary.LittleEndian.PutUint32(b[0:], attrData)
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)))
	b[8], b[9] = 1, byte(len(name))
	binary.LittleEndian.PutUint16(b[0x0A:], 0x40)
	for i, c := range name {
		binary.LittleEndian.PutUint16(b[0x40+2*i:], uint16(c))
	}
	binary.LittleEndian.PutUint64(b[0x18:], runs[len(runs)-1].VCN+runs[len(runs)-1].Len-1)
	binary.LittleEndian.PutUint16(b[0x20:], uint16(mp))
	copy(b[mp:], pairs)
	return b
}

// N4: parseAttribute checks the offsets encode relies on: a header of at
// least 0x40 bytes before the mapping pairs, the name before them, and
// resident values inside the attribute.
func TestParseAttributeChecksOffsets(t *testing.T) {
	good := testNonResident("abc", []Run{{VCN: 0, LCN: 10, Len: 5}})
	if a, err := parseAttribute(good); err != nil || a.Name != "abc" || len(a.Runs) != 1 {
		t.Fatalf("%+v %v", a, err)
	}
	for name, fn := range map[string]func(b []byte){
		"pairs inside the header":   func(b []byte) { binary.LittleEndian.PutUint16(b[0x20:], 0x38) },
		"name over the pairs":       func(b []byte) { binary.LittleEndian.PutUint16(b[0x0A:], 0x44) },
		"pairs past the end":        func(b []byte) { binary.LittleEndian.PutUint16(b[0x20:], uint16(len(b))) },
		"resident value past end":   func(b []byte) { b[8] = 0; binary.LittleEndian.PutUint32(b[0x10:], 1000) },
		"starting cluster too high": func(b []byte) { binary.LittleEndian.PutUint64(b[0x10:], 1<<63) },
	} {
		b := append([]byte(nil), good...)
		fn(b)
		if _, err := parseAttribute(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
