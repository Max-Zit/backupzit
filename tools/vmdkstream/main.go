// Command vmdkstream converts a raw disk image into a stream-optimized VMDK,
// the disk format of an OVA, following VMware's "Virtual Disk Format 1.1"
// specification: every grain carries a marker, the grain tables, the grain
// directory and a footer follow the data, and an end-of-stream marker closes
// the file. VMware Workstation reads such a disk strictly in this order and
// rejects files without the trailing metadata (the VMDKs of qemu-img).
//
//	vmdkstream -adapter lsilogic disk.raw backupzit-appliance-X-disk1.vmdk
//
// Used by packaging/appliance/build-images.sh; not shipped.
package main

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	sector      = 512
	grainSize   = 128 // sectors (64 KiB)
	gtEntries   = 512 // grain table entries
	gdAtEnd     = ^uint64(0)
	magic       = 0x564d444b // "KDMV"
	flagNewline = 1 << 0
	flagComp    = 1 << 16
	flagMarkers = 1 << 17

	markerEOS    = 0
	markerGT     = 1
	markerGD     = 2
	markerFooter = 3
)

type header struct {
	Magic              uint32
	Version            uint32
	Flags              uint32
	Capacity           uint64
	GrainSize          uint64
	DescriptorOffset   uint64
	DescriptorSize     uint64
	NumGTEsPerGT       uint32
	RGDOffset          uint64
	GDOffset           uint64
	OverHead           uint64
	UncleanShutdown    uint8
	SingleEndLineChar  byte
	NonEndLineChar     byte
	DoubleEndLineChar1 byte
	DoubleEndLineChar2 byte
	CompressAlgorithm  uint16
	Pad                [433]byte
}

func main() {
	adapter := flag.String("adapter", "lsilogic", "ddb.adapterType: lsilogic, buslogic, ide or legacyESX")
	verifyOnly := flag.Bool("verify", false, "read a stream-optimized VMDK strictly front to back and print the SHA256 of the disk it holds")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: vmdkstream [-adapter lsilogic] disk.raw out.vmdk\n       vmdkstream -verify disk.vmdk")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *verifyOnly && flag.NArg() == 1 {
		f, err := os.Open(flag.Arg(0))
		if err == nil {
			h := sha256.New()
			err = verify(bufio.NewReaderSize(f, 4<<20), h)
			_ = f.Close()
			if err == nil {
				fmt.Printf("%x\n", h.Sum(nil))
				return
			}
		}
		fmt.Fprintln(os.Stderr, "vmdkstream:", err)
		os.Exit(1)
	}
	if flag.NArg() != 2 {
		flag.Usage()
		os.Exit(2)
	}
	if err := convert(flag.Arg(0), flag.Arg(1), *adapter); err != nil {
		fmt.Fprintln(os.Stderr, "vmdkstream:", err)
		os.Exit(1)
	}
}

func convert(src, dst, adapter string) error {
	in, err := os.Open(src) // #nosec G304 -- path from the command line of a development tool
	if err != nil {
		return err
	}
	defer in.Close()
	size, err := in.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		return err
	}
	out, err := os.Create(dst) // #nosec G304 -- path from the command line of a development tool
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(out, 4<<20)
	err = write(w, bufio.NewReaderSize(in, 4<<20), size, filepath.Base(dst), adapter)
	if err == nil {
		err = w.Flush()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(dst)
	}
	return err
}

// write produces the stream-optimized VMDK of the size bytes read from r.
func write(w io.Writer, r io.Reader, size int64, name, adapter string) error {
	if size <= 0 || size%sector != 0 {
		return fmt.Errorf("image size %d is not a positive multiple of %d", size, sector)
	}
	capacity := (uint64(size)/sector + grainSize - 1) / grainSize * grainSize
	grains := capacity / grainSize
	numGTs := (grains + gtEntries - 1) / gtEntries

	desc := descriptor(capacity, name, adapter)
	descSectors := uint64((len(desc) + sector - 1) / sector)
	h := header{
		Magic: magic, Version: 3, Flags: flagNewline | flagComp | flagMarkers,
		Capacity: capacity, GrainSize: grainSize,
		DescriptorOffset: 1, DescriptorSize: descSectors,
		NumGTEsPerGT: gtEntries, GDOffset: gdAtEnd,
		OverHead:          1 + descSectors,
		SingleEndLineChar: '\n', NonEndLineChar: ' ', DoubleEndLineChar1: '\r', DoubleEndLineChar2: '\n',
		CompressAlgorithm: 1,
	}
	o := &out{w: w}
	if err := o.struct_(&h); err != nil {
		return err
	}
	if err := o.padded([]byte(desc)); err != nil {
		return err
	}

	gd := make([]uint32, numGTs)
	gt := make([]uint32, gtEntries)
	buf := make([]byte, grainSize*sector)
	var zbuf bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&zbuf, zlib.DefaultCompression)
	remaining := size
	for g := uint64(0); g < grains; g++ {
		n := int64(len(buf))
		if remaining < n {
			n = remaining
			clear(buf[n:])
		}
		if _, err := io.ReadFull(r, buf[:n]); err != nil {
			return fmt.Errorf("reading the image: %w", err)
		}
		remaining -= n
		if !zero(buf) {
			zbuf.Reset()
			zw.Reset(&zbuf)
			_, _ = zw.Write(buf)
			if err := zw.Close(); err != nil {
				return err
			}
			gt[g%gtEntries] = uint32(o.sector()) // #nosec G115 -- fields of a file format, within its limits
			m := make([]byte, 12, 12+zbuf.Len())
			binary.LittleEndian.PutUint64(m[0:], g*grainSize)
			binary.LittleEndian.PutUint32(m[8:], uint32(zbuf.Len())) // #nosec G115 -- fields of a file format, within its limits
			if err := o.padded(append(m, zbuf.Bytes()...)); err != nil {
				return err
			}
		}
		if g%gtEntries == gtEntries-1 || g == grains-1 {
			// Every grain table is written, also an empty one, so that each
			// grain directory entry points to a table.
			if err := o.marker(gtEntries*4/sector, markerGT); err != nil {
				return err
			}
			gd[g/gtEntries] = uint32(o.sector()) // #nosec G115 -- fields of a file format, within its limits
			if err := o.struct_(gt); err != nil {
				return err
			}
			clear(gt)
		}
	}

	gdSectors := (numGTs*4 + sector - 1) / sector
	if err := o.marker(gdSectors, markerGD); err != nil {
		return err
	}
	h.GDOffset = o.sector()
	gdBytes := make([]byte, gdSectors*sector)
	for i, v := range gd {
		binary.LittleEndian.PutUint32(gdBytes[i*4:], v)
	}
	if err := o.padded(gdBytes); err != nil {
		return err
	}
	if err := o.marker(1, markerFooter); err != nil {
		return err
	}
	if err := o.struct_(&h); err != nil {
		return err
	}
	return o.padded(make([]byte, sector)) // end-of-stream marker
}

// descriptor is the embedded text descriptor of the disk.
func descriptor(capacity uint64, name, adapter string) string {
	cid := make([]byte, 4)
	rand.Read(cid)
	cyl := capacity / (255 * 63)
	if cyl > 65535 {
		cyl = 65535
	}
	return fmt.Sprintf(`# Disk DescriptorFile
version=1
CID=%s
parentCID=ffffffff
createType="streamOptimized"

# Extent description
RW %d SPARSE "%s"

# The Disk Data Base
#DDB

ddb.adapterType = "%s"
ddb.geometry.cylinders = "%d"
ddb.geometry.heads = "255"
ddb.geometry.sectors = "63"
ddb.virtualHWVersion = "4"
`, hex.EncodeToString(cid), capacity, name, adapter, cyl)
}

func zero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// out writes whole sectors and counts them.
type out struct {
	w io.Writer
	n uint64 // bytes written
}

func (o *out) sector() uint64 { return o.n / sector }

// padded writes b and zeros up to the next sector boundary.
func (o *out) padded(b []byte) error {
	if _, err := o.w.Write(b); err != nil {
		return err
	}
	o.n += uint64(len(b))
	if rem := o.n % sector; rem != 0 {
		pad := make([]byte, sector-rem)
		if _, err := o.w.Write(pad); err != nil {
			return err
		}
		o.n += uint64(len(pad))
	}
	return nil
}

func (o *out) struct_(v any) error {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, v)
	return o.padded(b.Bytes())
}

// marker writes a metadata marker sector announcing numSectors of metadata.
func (o *out) marker(numSectors uint64, kind uint32) error {
	m := make([]byte, sector)
	binary.LittleEndian.PutUint64(m[0:], numSectors)
	binary.LittleEndian.PutUint32(m[12:], kind)
	return o.padded(m)
}

// verify reads a stream-optimized VMDK strictly front to back, the way
// VMware Workstation imports it, checks every marker, grain (with its
// Adler-32), grain table, the grain directory, the footer and the
// end-of-stream marker, and writes the disk's content to disk.
func verify(r io.Reader, disk io.Writer) error {
	sec := make([]byte, sector)
	read := func(b []byte) error {
		_, err := io.ReadFull(r, b)
		return err
	}
	if err := read(sec); err != nil {
		return fmt.Errorf("header: %w", err)
	}
	var h header
	_ = binary.Read(bytes.NewReader(sec), binary.LittleEndian, &h)
	if h.Magic != magic || h.Version != 3 || h.Flags&(flagComp|flagMarkers) != flagComp|flagMarkers ||
		h.GDOffset != gdAtEnd || h.CompressAlgorithm != 1 || h.GrainSize == 0 || h.NumGTEsPerGT == 0 ||
		h.DescriptorOffset != 1 || h.OverHead < 1+h.DescriptorSize {
		return fmt.Errorf("not a stream-optimized header with the grain directory at the end "+
			"(version %d, flags %#x, gdOffset %d, compression %d)", h.Version, h.Flags, int64(h.GDOffset), h.CompressAlgorithm) // #nosec G115 -- fields of a file format, within its limits
	}
	desc := make([]byte, h.DescriptorSize*sector)
	if err := read(desc); err != nil {
		return fmt.Errorf("descriptor: %w", err)
	}
	if !bytes.Contains(desc, []byte(`createType="streamOptimized"`)) {
		return errors.New(`descriptor without createType="streamOptimized"`)
	}
	if _, err := io.CopyN(io.Discard, r, int64(h.OverHead-1-h.DescriptorSize)*sector); err != nil { // #nosec G115 -- fields of a file format, within its limits
		return err
	}
	grainBytes := h.GrainSize * sector
	zeros := make([]byte, 1<<20)
	var written uint64 // bytes of the disk written so far
	fill := func(to uint64) error {
		for written < to {
			n := min(to-written, uint64(len(zeros)))
			if _, err := disk.Write(zeros[:n]); err != nil {
				return err
			}
			written += n
		}
		return nil
	}
	capBytes := h.Capacity * sector
	grainAt := map[uint64]uint64{} // marker sector -> lba
	var gts [][]byte
	var gtAt []uint64
	var gd []byte
	var gdAt uint64
	var footer *header
	pos := h.OverHead
	for {
		if err := read(sec); err != nil {
			return fmt.Errorf("sector %d: no end-of-stream marker (%w)", pos, err)
		}
		val := binary.LittleEndian.Uint64(sec)
		size := binary.LittleEndian.Uint32(sec[8:])
		if size != 0 {
			n := (12 + uint64(size) + sector - 1) / sector
			data := make([]byte, n*sector)
			copy(data, sec)
			if err := read(data[sector:]); err != nil {
				return fmt.Errorf("grain at sector %d: %w", pos, err)
			}
			if val%h.GrainSize != 0 || val*sector < written || val >= h.Capacity {
				return fmt.Errorf("grain at sector %d: lba %d out of order or range", pos, val)
			}
			zr, err := zlib.NewReader(bytes.NewReader(data[12 : 12+size]))
			if err != nil {
				return fmt.Errorf("grain at sector %d: %w", pos, err)
			}
			g, err := io.ReadAll(zr) // checks the Adler-32 too
			if err != nil || uint64(len(g)) != grainBytes {
				return fmt.Errorf("grain at sector %d: %d bytes, %v", pos, len(g), err)
			}
			if err := fill(val * sector); err != nil {
				return err
			}
			g = g[:min(grainBytes, capBytes-written)]
			if _, err := disk.Write(g); err != nil {
				return err
			}
			written += uint64(len(g))
			grainAt[pos] = val
			pos += n
			continue
		}
		kind := binary.LittleEndian.Uint32(sec[12:])
		pos++
		if kind == markerEOS {
			if val != 0 {
				return fmt.Errorf("end-of-stream marker at sector %d is not empty", pos-1)
			}
			if n, _ := r.Read(sec[:1]); n != 0 {
				return fmt.Errorf("data after the end-of-stream marker at sector %d", pos-1)
			}
			break
		}
		meta := make([]byte, val*sector)
		if err := read(meta); err != nil {
			return fmt.Errorf("metadata at sector %d: %w", pos, err)
		}
		switch kind {
		case markerGT:
			if uint64(len(meta)) < uint64(h.NumGTEsPerGT)*4 {
				return fmt.Errorf("grain table at sector %d too small", pos)
			}
			gts, gtAt = append(gts, meta), append(gtAt, pos)
		case markerGD:
			gd, gdAt = meta, pos
		case markerFooter:
			var f header
			_ = binary.Read(bytes.NewReader(meta), binary.LittleEndian, &f)
			footer = &f
		default:
			return fmt.Errorf("unknown marker type %d at sector %d", kind, pos-1)
		}
		pos += val
	}
	if gd == nil || footer == nil {
		return errors.New("grain directory or footer missing")
	}
	if footer.Magic != magic || footer.GDOffset != gdAt || footer.Capacity != h.Capacity || footer.GrainSize != h.GrainSize {
		return fmt.Errorf("footer does not match: gdOffset %d, grain directory at %d", footer.GDOffset, gdAt)
	}
	perGT := uint64(h.NumGTEsPerGT)
	numGTs := (h.Capacity/h.GrainSize + perGT - 1) / perGT
	if uint64(len(gd)) < numGTs*4 {
		return errors.New("grain directory too small")
	}
	tables := map[uint64][]byte{}
	for i, at := range gtAt {
		tables[at] = gts[i]
	}
	found := 0
	for i := uint64(0); i < numGTs; i++ {
		at := uint64(binary.LittleEndian.Uint32(gd[i*4:]))
		if at == 0 {
			continue
		}
		gt, ok := tables[at]
		if !ok {
			return fmt.Errorf("grain directory entry %d points to sector %d, which is no grain table", i, at)
		}
		for j := uint64(0); j < perGT; j++ {
			e := uint64(binary.LittleEndian.Uint32(gt[j*4:]))
			if e == 0 {
				continue
			}
			if lba, ok := grainAt[e]; !ok || lba != (i*perGT+j)*h.GrainSize {
				return fmt.Errorf("grain table %d entry %d points to sector %d, not to its grain", i, j, e)
			}
			found++
		}
	}
	if found != len(grainAt) {
		return fmt.Errorf("%d grains, %d in the grain tables", len(grainAt), found)
	}
	return fill(capBytes)
}
