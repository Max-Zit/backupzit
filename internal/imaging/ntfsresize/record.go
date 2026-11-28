package ntfsresize

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"
)

// Attribute types used here.
const (
	attrStandardInfo  = 0x10
	attrAttributeList = 0x20
	attrFileName      = 0x30
	attrVolumeInfo    = 0x70
	attrData          = 0x80
	attrBitmap        = 0xB0
	attrEnd           = 0xFFFFFFFF
)

// fixupStride is the size NTFS protects with one update sequence entry,
// independent of the disk's sector size.
const fixupStride = 512

// maxRecordSize is the largest file record size NTFS uses (and Open
// accepts). It also bounds every length written into a record.
const maxRecordSize = 64 << 10

// recordNumberMask selects the record number of a file reference; the top
// 16 bits are the sequence number.
const recordNumberMask = 1<<48 - 1

// Attribute is one attribute of a file record.
type Attribute struct {
	Type        uint32
	Name        string
	NonResident bool
	raw         []byte // header and (for resident attributes) value
	// Non-resident:
	StartVCN, LastVCN                    uint64
	AllocSize, DataSize, InitializedSize uint64
	Runs                                 []Run
	dirty                                bool
}

// Record is one MFT file record.
type Record struct {
	Number uint64
	InUse  bool
	// Extension is set for extension records; Base is then the number of
	// their base record. Base records have a zero base reference, so Base
	// alone cannot tell them from extensions of $MFT (record 0).
	Extension bool
	Base      uint64
	header    []byte // bytes before the first attribute
	Attrs     []*Attribute
	size      int
	dirty     bool
}

// usaRange returns the offset and entry count of the update sequence
// array of a multi-sector structure and checks that they fit the size:
// one entry for the sequence number and one per 512-byte stride, all
// inside the first stride (before its own protected last two bytes).
func usaRange(b []byte) (off, count int, err error) {
	if len(b) < fixupStride || len(b)%fixupStride != 0 {
		return 0, 0, errors.New("invalid multi-sector structure size")
	}
	off = int(binary.LittleEndian.Uint16(b[4:]))
	count = int(binary.LittleEndian.Uint16(b[6:]))
	if count != len(b)/fixupStride+1 || off&1 != 0 || off < 8 || off+2*count > fixupStride-2 {
		return 0, 0, errors.New("invalid update sequence")
	}
	return off, count, nil
}

// removeFixups checks and removes the update sequence of a multi-sector
// structure ("FILE" records, restart pages) in place.
func removeFixups(b []byte) error {
	usaOff, usaCount, err := usaRange(b)
	if err != nil {
		return err
	}
	usn := b[usaOff : usaOff+2]
	for i := 1; i < usaCount; i++ {
		end := i*fixupStride - 2
		if b[end] != usn[0] || b[end+1] != usn[1] {
			return fmt.Errorf("torn record: sector %d does not end with the update sequence number", i)
		}
		copy(b[end:end+2], b[usaOff+2*i:usaOff+2*i+2])
	}
	return nil
}

// applyFixups writes the update sequence into a multi-sector structure,
// saving the original sector ends in the update sequence array. The
// header must have passed removeFixups (records are only written from
// headers read from the source).
func applyFixups(b []byte) {
	usaOff, usaCount, err := usaRange(b)
	if err != nil {
		return
	}
	usn := binary.LittleEndian.Uint16(b[usaOff:])
	usn++
	if usn == 0 || usn == 0xFFFF {
		usn = 1
	}
	binary.LittleEndian.PutUint16(b[usaOff:], usn)
	for i := 1; i < usaCount; i++ {
		end := i*fixupStride - 2
		copy(b[usaOff+2*i:usaOff+2*i+2], b[end:end+2])
		binary.LittleEndian.PutUint16(b[end:], usn)
	}
}

// parseRecord parses a file record (fixups already removed).
func parseRecord(num uint64, b []byte) (*Record, error) {
	if len(b) < 0x30 || len(b) > maxRecordSize || string(b[0:4]) != "FILE" {
		return nil, fmt.Errorf("record %d: no FILE signature", num)
	}
	usaOff, usaCount, err := usaRange(b)
	if err != nil {
		return nil, fmt.Errorf("record %d: %w", num, err)
	}
	r := &Record{Number: num, size: len(b)}
	flags := binary.LittleEndian.Uint16(b[0x16:])
	r.InUse = flags&1 != 0
	ref := binary.LittleEndian.Uint64(b[0x20:])
	r.Extension = ref != 0
	r.Base = ref & recordNumberMask
	first := int(binary.LittleEndian.Uint16(b[0x14:]))
	used := int(binary.LittleEndian.Uint32(b[0x18:]))
	// The update sequence array must lie in the header: encode copies the
	// header as it is and applyFixups writes the array there. It must also
	// start after the header's own fields (0x2A in NTFS 3.0, 0x30 in 3.1):
	// an array over the attribute offset or the used size would change
	// them when applyFixups writes it.
	if first < 0x30 || first >= len(b) || used > len(b) || used < first || usaOff < 0x2a || usaOff+2*usaCount > first {
		return nil, fmt.Errorf("record %d: invalid header", num)
	}
	r.header = append([]byte(nil), b[:first]...)
	for off := first; ; {
		if off+8 > used {
			return nil, fmt.Errorf("record %d: attributes run past the end", num)
		}
		t := binary.LittleEndian.Uint32(b[off:])
		if t == attrEnd {
			break
		}
		l := int(binary.LittleEndian.Uint32(b[off+4:]))
		if l < 0x18 || l > used-off {
			return nil, fmt.Errorf("record %d: invalid attribute length", num)
		}
		a, err := parseAttribute(b[off : off+l])
		if err != nil {
			return nil, fmt.Errorf("record %d: %w", num, err)
		}
		r.Attrs = append(r.Attrs, a)
		off += l
	}
	return r, nil
}

// parseAttribute parses one attribute (b holds exactly its bytes). It
// checks every offset that encode relies on: a non-resident header of at
// least 0x40 bytes before the mapping pairs, and the name before them.
func parseAttribute(b []byte) (*Attribute, error) {
	a := &Attribute{Type: binary.LittleEndian.Uint32(b), NonResident: b[8] != 0, raw: append([]byte(nil), b...)}
	nl := int(b[9])
	no := int(binary.LittleEndian.Uint16(b[0x0A:]))
	if nl > 0 {
		if no+2*nl > len(b) {
			return nil, errors.New("attribute name out of range")
		}
		u := make([]uint16, nl)
		for i := range u {
			u[i] = binary.LittleEndian.Uint16(b[no+2*i:])
		}
		a.Name = string(utf16.Decode(u))
	}
	if !a.NonResident {
		vl := uint64(binary.LittleEndian.Uint32(b[0x10:]))
		vo := uint64(binary.LittleEndian.Uint16(b[0x14:]))
		if vo < 0x18 || vo+vl > uint64(len(b)) {
			return nil, errors.New("resident attribute value out of range")
		}
		return a, nil
	}
	if len(b) < 0x40 {
		return nil, errors.New("non-resident attribute header too short")
	}
	a.StartVCN = binary.LittleEndian.Uint64(b[0x10:])
	a.LastVCN = binary.LittleEndian.Uint64(b[0x18:])
	mp := int(binary.LittleEndian.Uint16(b[0x20:]))
	a.AllocSize = binary.LittleEndian.Uint64(b[0x28:])
	a.DataSize = binary.LittleEndian.Uint64(b[0x30:])
	a.InitializedSize = binary.LittleEndian.Uint64(b[0x38:])
	// encode keeps the bytes before the mapping pairs and rewrites the
	// size fields at 0x18-0x3F: both must come before the pairs.
	if mp < 0x40 || mp >= len(b) {
		return nil, errors.New("mapping pairs out of range")
	}
	if nl > 0 && no+2*nl > mp {
		return nil, errors.New("attribute name overlaps the mapping pairs")
	}
	runs, err := decodeRuns(b[mp:], a.StartVCN)
	if err != nil {
		return nil, err
	}
	a.Runs = runs
	return a, nil
}

// encode rebuilds the attribute: new mapping pairs and sizes for changed
// non-resident attributes; other attributes keep their bytes. The length
// field is set by Record.encode, which knows it fits.
func (a *Attribute) encode() []byte {
	if !a.NonResident || !a.dirty {
		return a.raw
	}
	mp := int(binary.LittleEndian.Uint16(a.raw[0x20:]))
	out := make([]byte, 0, mp+pairsLen(a.Runs)+7)
	out = append(out, a.raw[:mp]...)
	out = append(out, encodeRuns(a.Runs)...)
	for len(out)%8 != 0 {
		out = append(out, 0)
	}
	binary.LittleEndian.PutUint64(out[0x18:], a.LastVCN)
	binary.LittleEndian.PutUint64(out[0x28:], a.AllocSize)
	binary.LittleEndian.PutUint64(out[0x30:], a.DataSize)
	binary.LittleEndian.PutUint64(out[0x38:], a.InitializedSize)
	return out
}

// encodedLen is len(a.encode()) with runs instead of a.Runs, computed
// without building the bytes.
func (a *Attribute) encodedLen(runs []Run) int {
	mp := int(binary.LittleEndian.Uint16(a.raw[0x20:]))
	return (mp + pairsLen(runs) + 7) &^ 7
}

// errRecordFull is returned when moved clusters make a record's mapping
// pairs grow beyond the record (no attribute list is created).
var errRecordFull = errors.New("the attributes do not fit after moving clusters")

// encode rebuilds the record (without fixups); it fails when the
// attributes no longer fit.
func (r *Record) encode() ([]byte, error) {
	b := make([]byte, r.size)
	copy(b, r.header)
	off := len(r.header)
	for _, a := range r.Attrs {
		e := a.encode()
		n := len(e)
		if n > maxRecordSize || off+n+8 > r.size {
			return nil, fmt.Errorf("record %d: %w", r.Number, errRecordFull)
		}
		copy(b[off:], e)
		binary.LittleEndian.PutUint32(b[off+4:], uint32(n))
		off += n
	}
	binary.LittleEndian.PutUint32(b[off:], attrEnd)
	off += 8
	if off > maxRecordSize {
		return nil, fmt.Errorf("record %d: %w", r.Number, errRecordFull)
	}
	binary.LittleEndian.PutUint32(b[0x18:], uint32(off))
	return b, nil
}

// clone returns a deep copy that can be changed without touching r.
func (r *Record) clone() *Record {
	c := *r
	c.header = append([]byte(nil), r.header...)
	c.Attrs = make([]*Attribute, len(r.Attrs))
	for i, a := range r.Attrs {
		ac := *a
		ac.raw = append([]byte(nil), a.raw...)
		ac.Runs = append([]Run(nil), a.Runs...)
		c.Attrs[i] = &ac
	}
	return &c
}

// unnamed finds the first attribute of a type without a name.
func (r *Record) unnamed(t uint32) *Attribute {
	for _, a := range r.Attrs {
		if a.Type == t && a.Name == "" {
			return a
		}
	}
	return nil
}

// named finds the first attribute of a type with the given name.
func (r *Record) named(t uint32, name string) *Attribute {
	for _, a := range r.Attrs {
		if a.Type == t && a.Name == name {
			return a
		}
	}
	return nil
}

// residentValue returns the value of a resident attribute (parseAttribute
// checked that it lies inside the attribute).
func (a *Attribute) residentValue() []byte {
	if a.NonResident || len(a.raw) < 0x18 {
		return nil
	}
	l := uint64(binary.LittleEndian.Uint32(a.raw[0x10:]))
	o := uint64(binary.LittleEndian.Uint16(a.raw[0x14:]))
	if o+l > uint64(len(a.raw)) {
		return nil
	}
	return a.raw[o : o+l]
}

// fileName returns the parent directory and the name of a $FILE_NAME
// attribute.
func (a *Attribute) fileName() (parent uint64, name string, ok bool) {
	if a.Type != attrFileName {
		return 0, "", false
	}
	v := a.residentValue()
	if len(v) < 0x42 {
		return 0, "", false
	}
	n := int(v[0x40])
	if len(v) < 0x42+2*n {
		return 0, "", false
	}
	u := make([]uint16, n)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(v[0x42+2*i:])
	}
	return binary.LittleEndian.Uint64(v) & recordNumberMask, string(utf16.Decode(u)), true
}

// hasName tells whether the record has a $FILE_NAME with this name
// (compared without case, as Windows does) in the given directory.
func (r *Record) hasName(dir uint64, name string) bool {
	for _, a := range r.Attrs {
		if p, n, ok := a.fileName(); ok && p == dir && strings.EqualFold(n, name) {
			return true
		}
	}
	return false
}

// isBadClus tells whether a is $BadClus:$Bad, a sparse stream as long as
// the volume that owns no clusters (bad clusters are refused).
func isBadClus(rec *Record, a *Attribute) bool {
	return rec.Number == recBadClus && a.Type == attrData && a.Name == "$Bad"
}

// equalOutsideUSA compares two records of the same layout, ignoring the
// update sequence array (its sequence number changes with every write).
func equalOutsideUSA(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	off, count, err := usaRange(a)
	if err != nil {
		return bytes.Equal(a, b)
	}
	end := off + 2*count
	return bytes.Equal(a[:off], b[:off]) && bytes.Equal(a[end:], b[end:])
}
