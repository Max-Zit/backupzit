package vmware

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/max-zit/backupzit/internal/repo"
)

// Restores through the vSphere API cannot write into a disk at offsets, so a
// disk is uploaded as a monolithicSparse VMDK (only the blocks that hold
// data) and converted into a thin VMFS disk with CopyVirtualDisk.

const (
	sparseGrainSectors = 128 // 64 KiB grains
	sparseGrainBytes   = sparseGrainSectors * 512
	sparseGTEs         = 512 // grain table entries per grain table
	sparseDescSectors  = 20
)

// sparseLayout places the metadata and the grains of a sparse VMDK.
type sparseLayout struct {
	capacity  uint64   // sectors
	grains    []uint64 // grain numbers with data, ascending
	gtCount   uint64
	gdSectors uint64
	rgdOffset uint64
	gdOffset  uint64
	overhead  uint64 // sectors before the first grain
	desc      []byte
}

func newSparseLayout(size uint64, grains []uint64, adapter string) *sparseLayout {
	l := &sparseLayout{capacity: (size + 511) / 512, grains: grains}
	totalGrains := (l.capacity + sparseGrainSectors - 1) / sparseGrainSectors
	l.gtCount = (totalGrains + sparseGTEs - 1) / sparseGTEs
	l.gdSectors = (l.gtCount*4 + 511) / 512
	gtSectors := l.gtCount * sparseGTEs * 4 / 512
	// Header, descriptor, then the redundant directory and its tables, then
	// the directory and its tables; grains start on a grain boundary.
	l.rgdOffset = 1 + sparseDescSectors
	l.gdOffset = l.rgdOffset + l.gdSectors + gtSectors
	end := l.gdOffset + l.gdSectors + gtSectors
	l.overhead = (end + sparseGrainSectors - 1) / sparseGrainSectors * sparseGrainSectors
	cyl := l.capacity / (255 * 63)
	if cyl < 1 {
		cyl = 1
	}
	l.desc = []byte(fmt.Sprintf("# Disk DescriptorFile\nversion=1\nCID=fffffffe\nparentCID=ffffffff\ncreateType=\"monolithicSparse\"\n\n"+
		"# Extent description\nRW %d SPARSE \"disk.vmdk\"\n\n# The Disk Data Base\n#DDB\n\n"+
		"ddb.adapterType = \"%s\"\nddb.geometry.cylinders = \"%d\"\nddb.geometry.heads = \"255\"\nddb.geometry.sectors = \"63\"\nddb.virtualHWVersion = \"13\"\n",
		l.capacity, adapter, cyl))
	return l
}

// Size is the length of the VMDK file in bytes.
func (l *sparseLayout) Size() int64 {
	return int64(l.overhead*512) + int64(len(l.grains))*sparseGrainBytes
}

// metadata returns everything before the first grain.
func (l *sparseLayout) metadata() []byte {
	b := make([]byte, l.overhead*512)
	h := b[:512]
	binary.LittleEndian.PutUint32(h[0:], 0x564d444b) // "KDMV"
	binary.LittleEndian.PutUint32(h[4:], 1)
	binary.LittleEndian.PutUint32(h[8:], 3) // valid newline test, redundant grain table
	binary.LittleEndian.PutUint64(h[12:], l.capacity)
	binary.LittleEndian.PutUint64(h[20:], sparseGrainSectors)
	binary.LittleEndian.PutUint64(h[28:], 1)
	binary.LittleEndian.PutUint64(h[36:], sparseDescSectors)
	binary.LittleEndian.PutUint32(h[44:], sparseGTEs)
	binary.LittleEndian.PutUint64(h[48:], l.rgdOffset)
	binary.LittleEndian.PutUint64(h[56:], l.gdOffset)
	binary.LittleEndian.PutUint64(h[64:], l.overhead)
	h[72] = 0 // clean
	h[73], h[74], h[75], h[76] = '\n', ' ', '\r', '\n'
	copy(b[512:512*(1+sparseDescSectors)], l.desc)
	// Grain tables: entry = sector of the grain, 0 = not allocated.
	gts := make([]uint32, l.gtCount*sparseGTEs)
	for i, g := range l.grains {
		gts[g] = uint32(l.overhead + uint64(i)*sparseGrainSectors)
	}
	write := func(gd uint64) {
		gtStart := gd + l.gdSectors
		for t := uint64(0); t < l.gtCount; t++ {
			at := gtStart + t*sparseGTEs*4/512
			binary.LittleEndian.PutUint32(b[gd*512+t*4:], uint32(at))
			for e := uint64(0); e < sparseGTEs; e++ {
				binary.LittleEndian.PutUint32(b[at*512+e*4:], gts[t*sparseGTEs+e])
			}
		}
	}
	write(l.rgdOffset)
	write(l.gdOffset)
	return b
}

// sparseImage lists the grains of a disk image that hold data.
func sparseImage(ctx context.Context, r *repo.Repository, img *repo.DiskImage) ([]repo.ID, []uint64, error) {
	if len(img.Partitions) != 1 {
		return nil, nil, errors.New("not a whole-disk image")
	}
	p := &img.Partitions[0]
	if p.BlockSize == 0 || p.BlockSize%sparseGrainBytes != 0 {
		return nil, nil, fmt.Errorf("unsupported block size %d", p.BlockSize)
	}
	ids, err := r.LoadBlockMap(ctx, p)
	if err != nil {
		return nil, nil, err
	}
	per := uint64(p.BlockSize / sparseGrainBytes)
	var grains []uint64
	for i, id := range ids {
		if id.IsNull() {
			continue
		}
		off := uint64(i) * uint64(p.BlockSize)
		n := min(uint64(p.BlockSize), p.Length-off)
		for g := uint64(0); g < (n+sparseGrainBytes-1)/sparseGrainBytes; g++ {
			grains = append(grains, uint64(i)*per+g)
		}
	}
	return ids, grains, nil
}

// sparseReader streams the VMDK: metadata, then the grains block by block.
type sparseReader struct {
	ctx      context.Context
	r        *repo.Repository
	p        *repo.PartitionImage
	ids      []repo.ID
	head     []byte
	block    int // next block index
	buf      []byte
	progress func(uint64)
}

func (s *sparseReader) Read(b []byte) (int, error) {
	for len(s.buf) == 0 {
		if s.head != nil {
			s.buf, s.head = s.head, nil
			break
		}
		for s.block < len(s.ids) && s.ids[s.block].IsNull() {
			off := uint64(s.block) * uint64(s.p.BlockSize)
			s.progress(min(uint64(s.p.BlockSize), s.p.Length-off))
			s.block++
		}
		if s.block >= len(s.ids) {
			return 0, io.EOF
		}
		if err := s.ctx.Err(); err != nil {
			return 0, err
		}
		i := s.block
		s.block++
		data, err := s.r.LoadBlob(s.ctx, repo.DataBlob, s.ids[i])
		if err != nil {
			return 0, fmt.Errorf("block %d: %w", i, err)
		}
		off := uint64(i) * uint64(s.p.BlockSize)
		n := min(uint64(s.p.BlockSize), s.p.Length-off)
		if uint64(len(data)) != n {
			return 0, fmt.Errorf("block %d has %d bytes, expected %d", i, len(data), n)
		}
		s.progress(n)
		// The last grain of the disk is padded to a whole grain.
		if pad := (sparseGrainBytes - n%sparseGrainBytes) % sparseGrainBytes; pad > 0 {
			data = append(data, make([]byte, pad)...)
		}
		s.buf = data
	}
	n := copy(b, s.buf)
	s.buf = s.buf[n:]
	return n, nil
}

// uploadSparse writes a disk image as a monolithicSparse VMDK to p.
func (c *Client) uploadSparse(ctx context.Context, r *repo.Repository, img *repo.DiskImage, p dsPath, adapter string, progress func(uint64)) error {
	ids, grains, err := sparseImage(ctx, r, img)
	if err != nil {
		return err
	}
	l := newSparseLayout(img.Size, grains, adapter)
	if progress == nil {
		progress = func(uint64) {}
	}
	body := &sparseReader{ctx: ctx, r: r, p: &img.Partitions[0], ids: ids, head: l.metadata(), progress: progress}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.fileURL(p), body)
	if err != nil {
		return err
	}
	req.ContentLength = l.Size()
	req.Header.Set("Content-Type", "application/octet-stream")
	return c.vim.Do(ctx, req, func(resp *http.Response) error {
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
			return fmt.Errorf("upload %s: %s", p, resp.Status)
		}
		return nil
	})
}
