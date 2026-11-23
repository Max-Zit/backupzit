package ntfsresize

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Attribute types used here.
const (
	attrStandardInfo  = 0x10
	attrAttributeList = 0x20
	attrVolumeInfo    = 0x70
	attrData          = 0x80
	attrBitmap        = 0xB0
	attrEnd           = 0xFFFFFFFF
)

// fixupStride is the size NTFS protects with one update sequence entry,
// independent of the disk's sector size.
const fixupStride = 512

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

// removeFixups checks and removes the update sequence of a multi-sector
// structure ("FILE" records, index blocks) in place.
func removeFixups(b []byte) error {
	usaOff := int(binary.LittleEndian.Uint16(b[4:]))
	usaCount := int(binary.LittleEndian.Uint16(b[6:]))
	if usaCount < 1 || usaOff+2*usaCount > len(b) || (usaCount-1)*fixupStride > len(b) {
		return errors.New("invalid update sequence")
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
// saving the original sector ends in the update sequence array.
func applyFixups(b []byte) {
	usaOff := int(binary.LittleEndian.Uint16(b[4:]))
	usaCount := int(binary.LittleEndian.Uint16(b[6:]))
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
	if string(b[0:4]) != "FILE" {
		return nil, fmt.Errorf("record %d: no FILE signature", num)
	}
	r := &Record{Number: num, size: len(b)}
	flags := binary.LittleEndian.Uint16(b[0x16:])
	r.InUse = flags&1 != 0
	ref := binary.LittleEndian.Uint64(b[0x20:])
	r.Extension = ref != 0
	r.Base = ref & 0xFFFFFFFFFFFF
	first := int(binary.LittleEndian.Uint16(b[0x14:]))
	used := int(binary.LittleEndian.Uint32(b[0x18:]))
	if first < 0x30 || first >= len(b) || used > len(b) || used < first {
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
		if l < 0x18 || off+l > used {
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

func parseAttribute(b []byte) (*Attribute, error) {
	a := &Attribute{Type: binary.LittleEndian.Uint32(b), NonResident: b[8] != 0, raw: append([]byte(nil), b...)}
	nl := int(b[9])
	no := int(binary.LittleEndian.Uint16(b[0x0A:]))
	if nl > 0 {
		if no+2*nl > len(b) {
			return nil, errors.New("attribute name out of range")
		}
		u := make([]rune, nl)
		for i := range u {
			u[i] = rune(binary.LittleEndian.Uint16(b[no+2*i:]))
		}
		a.Name = string(u)
	}
	if !a.NonResident {
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
	if mp >= len(b) {
		return nil, errors.New("mapping pairs out of range")
	}
	runs, err := decodeRuns(b[mp:], a.StartVCN)
	if err != nil {
		return nil, err
	}
	a.Runs = runs
	return a, nil
}

// encode rebuilds the attribute: new mapping pairs and sizes for
// non-resident attributes; resident attributes keep their bytes.
func (a *Attribute) encode() []byte {
	if !a.NonResident || !a.dirty {
		return a.raw
	}
	mp := int(binary.LittleEndian.Uint16(a.raw[0x20:]))
	out := append([]byte(nil), a.raw[:mp]...)
	out = append(out, encodeRuns(a.Runs)...)
	for len(out)%8 != 0 {
		out = append(out, 0)
	}
	binary.LittleEndian.PutUint32(out[4:], uint32(len(out))) // #nosec G115 -- attribute length is below the record size
	binary.LittleEndian.PutUint64(out[0x18:], a.LastVCN)
	binary.LittleEndian.PutUint64(out[0x28:], a.AllocSize)
	binary.LittleEndian.PutUint64(out[0x30:], a.DataSize)
	binary.LittleEndian.PutUint64(out[0x38:], a.InitializedSize)
	return out
}

// encode rebuilds the record (without fixups); it fails when the
// attributes no longer fit.
func (r *Record) encode() ([]byte, error) {
	b := make([]byte, r.size)
	copy(b, r.header)
	off := len(r.header)
	for _, a := range r.Attrs {
		e := a.encode()
		if off+len(e)+8 > r.size {
			return nil, fmt.Errorf("record %d: the attributes do not fit after moving clusters", r.Number)
		}
		copy(b[off:], e)
		off += len(e)
	}
	binary.LittleEndian.PutUint32(b[off:], attrEnd)
	off += 8
	binary.LittleEndian.PutUint32(b[0x18:], uint32(off)) // #nosec G115 -- below the record size
	return b, nil
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

// residentValue returns the value of a resident attribute.
func (a *Attribute) residentValue() []byte {
	if a.NonResident || len(a.raw) < 0x18 {
		return nil
	}
	l := int(binary.LittleEndian.Uint32(a.raw[0x10:]))
	o := int(binary.LittleEndian.Uint16(a.raw[0x14:]))
	if o+l > len(a.raw) {
		return nil
	}
	return a.raw[o : o+l]
}
