package ntfsresize

import (
	"encoding/binary"
	"slices"
	"testing"
)

// listEntry builds an $ATTRIBUTE_LIST entry for an unnamed attribute.
func listEntry(typ uint32, vcn, ref uint64, id uint16) []byte {
	e := make([]byte, 0x20)
	binary.LittleEndian.PutUint32(e[0:], typ)
	binary.LittleEndian.PutUint16(e[4:], 0x20)
	e[7] = 0x1A
	binary.LittleEndian.PutUint64(e[8:], vcn)
	binary.LittleEndian.PutUint64(e[0x10:], ref)
	binary.LittleEndian.PutUint16(e[0x18:], id)
	return e
}

// splitMFT gives $MFT an attribute list, as a heavily fragmented $MFT
// has: record 0 keeps $DATA VCN 0..split-1, extension record ext holds
// VCN split..end (the review's probe 3). With dup, the list names the
// extension record twice.
func splitMFT(t *testing.T, src []byte, split, ext uint64, dup bool) []byte {
	t.Helper()
	v := mustOpen(t, src)
	r0 := v.records[recMFT].clone()
	d := r0.unnamed(attrData)
	ref0 := uint64(binary.LittleEndian.Uint16(r0.header[0x10:])) << 48
	var seg1, seg2 []Run
	for _, r := range d.Runs {
		switch {
		case r.VCN+r.Len <= split:
			seg1 = append(seg1, r)
		case r.VCN >= split:
			seg2 = append(seg2, r)
		default:
			k := split - r.VCN
			seg1 = append(seg1, Run{VCN: r.VCN, LCN: r.LCN, Len: k})
			seg2 = append(seg2, Run{VCN: split, LCN: r.LCN + k, Len: r.Len - k})
		}
	}
	last := d.LastVCN
	d.Runs, d.LastVCN, d.dirty = seg1, split-1, true

	// The extension record, with the header of record 39 (an extension
	// record of the sample).
	r39, err := v.readRecord(39)
	if err != nil {
		t.Fatal(err)
	}
	hdr := append([]byte(nil), r39.header...)
	binary.LittleEndian.PutUint64(hdr[0x20:], ref0) // base: $MFT
	binary.LittleEndian.PutUint16(hdr[0x16:], 1)    // in use
	binary.LittleEndian.PutUint16(hdr[0x10:], 1)    // sequence number
	binary.LittleEndian.PutUint16(hdr[0x28:], 1)    // next attribute id
	binary.LittleEndian.PutUint32(hdr[0x2C:], uint32(ext))
	raw := append([]byte(nil), d.raw[:binary.LittleEndian.Uint16(d.raw[0x20:])]...)
	binary.LittleEndian.PutUint64(raw[0x10:], split)
	binary.LittleEndian.PutUint16(raw[0x0E:], 0)
	// Later segments have zero sizes.
	ea := &Attribute{Type: attrData, NonResident: true, raw: raw, StartVCN: split, LastVCN: last, Runs: seg2, dirty: true}
	er := &Record{Number: ext, InUse: true, Extension: true, header: hdr, Attrs: []*Attribute{ea}, size: int(v.RecordSize), dirty: true}

	// The attribute list in record 0, after $STANDARD_INFORMATION.
	nextID := binary.LittleEndian.Uint16(r0.header[0x28:])
	var list []byte
	for _, a := range r0.Attrs {
		list = append(list, listEntry(a.Type, a.StartVCN, ref0, binary.LittleEndian.Uint16(a.raw[0x0E:]))...)
		if a == d {
			list = append(list, listEntry(attrData, split, ext|1<<48, 0)...)
			if dup {
				list = append(list, listEntry(attrData, split, ext|1<<48, 0)...)
			}
		}
	}
	alraw := make([]byte, 0x18+len(list))
	binary.LittleEndian.PutUint32(alraw[0:], attrAttributeList)
	binary.LittleEndian.PutUint32(alraw[4:], uint32(len(alraw)))
	binary.LittleEndian.PutUint16(alraw[0x0A:], 0x18)
	binary.LittleEndian.PutUint16(alraw[0x0E:], nextID)
	binary.LittleEndian.PutUint32(alraw[0x10:], uint32(len(list)))
	binary.LittleEndian.PutUint16(alraw[0x14:], 0x18)
	copy(alraw[0x18:], list)
	binary.LittleEndian.PutUint16(r0.header[0x28:], nextID+1)
	var attrs []*Attribute
	for _, a := range r0.Attrs {
		attrs = append(attrs, a)
		if a.Type == attrStandardInfo {
			attrs = append(attrs, &Attribute{Type: attrAttributeList, raw: alraw})
		}
	}
	r0.Attrs = attrs

	img := append([]byte(nil), src...)
	mirr := v.records[recMFTMirr].unnamed(attrData).Runs
	writeRecord(t, v, img, v.mftRuns, mirr, r0)
	writeRecord(t, v, img, v.mftRuns, mirr, er)
	// $MFT:$BITMAP: the extension record is in use.
	mb := r0.unnamed(attrBitmap)
	lcn, err := lcnOf(mb.Runs, ext/8/v.ClusterSize)
	if err != nil {
		t.Fatal(err)
	}
	img[lcn*v.ClusterSize+(ext/8)%v.ClusterSize] |= 1 << (ext % 8)
	return img
}

// The review's probe 3: $MFT with an attribute list and an extension
// record. Variant A moves only the extension's segment, variant B both
// (the boot sector's $MFT position changes). go-ntfs checks every result.
func TestShrinkMFTWithAttributeList(t *testing.T) {
	src := testImage(t)
	orig := fileHashes(t, src)
	split := splitMFT(t, src, 32, 16, false)
	sameFiles(t, orig, fileHashes(t, split))
	v := checkConsistent(t, split)
	if len(v.mftRuns) != 1 || v.mftClusters != 256 {
		t.Fatalf("$MFT runs %v (%d clusters)", v.mftRuns, v.mftClusters)
	}
	segA := relocate(t, split, 16, 0, []Run{{VCN: 32, LCN: 9000, Len: 224}})
	sameFiles(t, orig, fileHashes(t, segA))
	segB := relocate(t, segA, 0, 3, []Run{{VCN: 0, LCN: 8700, Len: 32}})
	sameFiles(t, orig, fileHashes(t, segB))
	if lcn := binary.LittleEndian.Uint64(segB[0x30:]); lcn != 8700 {
		t.Fatalf("$MFT at %d", lcn)
	}
	for name, img := range map[string][]byte{"A": segA, "B": segB} {
		for _, size := range []uint64{6 << 20, 5 << 20, 4300 * 1024} {
			out, p := shrink(t, img, size, snapshot)
			nv := checkConsistent(t, out)
			sameFiles(t, orig, fileHashes(t, out))
			for _, e := range verifyAgainst(t, img, out, p.NewClusters) {
				t.Errorf("variant %s size %d: %s", name, size, e)
			}
			if l := binary.LittleEndian.Uint64(out[0x30:]); l >= nv.Clusters || l != nv.mftRuns[0].LCN {
				t.Errorf("variant %s size %d: boot sector puts $MFT at %d, runs %v", name, size, l, nv.mftRuns)
			}
		}
	}
}

// N3: two attribute list entries naming the same extension record give
// its segments once.
func TestOpenReadsEachMFTExtensionOnce(t *testing.T) {
	src := testImage(t)
	want := mustOpen(t, relocate(t, splitMFT(t, src, 32, 16, false), 16, 0, []Run{{VCN: 32, LCN: 9000, Len: 224}}))
	dup := relocate(t, splitMFT(t, src, 32, 16, true), 16, 0, []Run{{VCN: 32, LCN: 9000, Len: 224}})
	got := checkConsistent(t, dup)
	if !slices.Equal(got.mftRuns, want.mftRuns) || got.mftClusters != want.mftClusters {
		t.Errorf("$MFT runs %v, want %v", got.mftRuns, want.mftRuns)
	}
	shrink(t, dup, 5<<20, snapshot)
}
