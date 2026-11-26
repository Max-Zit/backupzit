package vmfs

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"hash/fnv"
	"io"
	"io/fs"
	"math"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/cespare/xxhash/v2"
	"golang.org/x/crypto/blake2b"
)

// A read-only btrfs reader, written for BackupZit from the on-disk format.
// It reads file systems on one device (SINGLE, DUP and mirrored chunks),
// starts at the top-level subvolume (ID 5) and shows the subvolumes and
// snapshots linked below it as folders, the way the file system looks
// when the top level is mounted. Files compressed with zlib, LZO or zstd
// are decompressed. Every tree block is checked (checksum, address, file
// system ID, level), and a DUP copy is used when the first one is bad.

const (
	btrfsSuperOffset = 0x10000
	btrfsSuperSize   = 4096
	btrfsMagic       = "_BHRfS_M"
	btrfsHeaderSize  = 101
	btrfsItemSize    = 25
	btrfsKeyPtrSize  = 33
	btrfsMaxLevel    = 7

	btrfsFSTree   = 5
	btrfsTopDirID = 256

	btrfsInodeItem  = 1
	btrfsDirItem    = 84
	btrfsDirIndex   = 96
	btrfsExtentData = 108
	btrfsRootItem   = 132
	btrfsRootRef    = 156
	btrfsChunkItem  = 228

	// Block group profile bits of a chunk.
	btrfsRAID0  = 1 << 3
	btrfsRAID10 = 1 << 6
	btrfsRAID5  = 1 << 7
	btrfsRAID6  = 1 << 8

	// Incompatible features this reader does not understand.
	btrfsMetadataUUID   = 1 << 10
	btrfsExtentTreeV2   = 1 << 13
	btrfsRaidStripeTree = 1 << 14

	// Compressed extents hold at most 128 KiB (BTRFS_MAX_COMPRESSED).
	btrfsMaxCompressed = 128 << 10
	btrfsMaxInline     = 64 << 10
	btrfsNodeCache     = 256
)

type bkey struct {
	obj uint64
	typ uint8
	off uint64
}

func (a bkey) less(b bkey) bool {
	if a.obj != b.obj {
		return a.obj < b.obj
	}
	if a.typ != b.typ {
		return a.typ < b.typ
	}
	return a.off < b.off
}

func readKey(b []byte) bkey {
	return bkey{obj: binary.LittleEndian.Uint64(b), typ: b[8], off: binary.LittleEndian.Uint64(b[9:])}
}

type btrfsSuper struct {
	fsid       [16]byte // what tree block headers carry
	root       uint64
	rootLevel  uint8
	chunkRoot  uint64
	chunkLevel uint8
	sectorSize uint32
	nodeSize   uint32
	csumType   uint16
	devid      uint64
	sysChunks  []byte
}

type btrfsChunk struct {
	logical, length uint64
	stripes         []uint64 // physical offsets of the copies on this device
}

type btrfsTree struct {
	id     uint64
	bytenr uint64
	level  uint8
	dirID  uint64 // the subvolume's top folder
}

type btrfsNode struct {
	level uint8
	n     int
	b     []byte
}

func (n *btrfsNode) key(i int) bkey {
	if n.level == 0 {
		return readKey(n.b[btrfsHeaderSize+i*btrfsItemSize:])
	}
	return readKey(n.b[btrfsHeaderSize+i*btrfsKeyPtrSize:])
}

func (n *btrfsNode) ptr(i int) uint64 {
	return binary.LittleEndian.Uint64(n.b[btrfsHeaderSize+i*btrfsKeyPtrSize+17:])
}

func (n *btrfsNode) item(i int) []byte {
	h := n.b[btrfsHeaderSize+i*btrfsItemSize:]
	off, size := binary.LittleEndian.Uint32(h[17:]), binary.LittleEndian.Uint32(h[21:])
	return n.b[btrfsHeaderSize+int(off) : btrfsHeaderSize+int(off)+int(size)]
}

type btrfsFS struct {
	r      io.ReaderAt
	sb     btrfsSuper
	chunks []btrfsChunk // sorted by logical address
	cache  map[uint64]*btrfsNode
	order  []uint64
	trees  map[uint64]*btrfsTree // nil: no such subvolume
	top    *btrfsTree

	extKey  [3]uint64 // the extent in ext (address, length, compression)
	extData []byte
}

var errBtrfsDamaged = errors.New("damaged btrfs file system")

func btrfsErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", errBtrfsDamaged, fmt.Sprintf(format, a...))
}

// isBtrfs reports a btrfs superblock in a volume.
func isBtrfs(r io.ReaderAt, size int64) (label string, ok bool) {
	if size < btrfsSuperOffset+btrfsSuperSize {
		return "", false
	}
	b := make([]byte, btrfsSuperSize)
	if _, err := r.ReadAt(b, btrfsSuperOffset); err != nil && err != io.EOF {
		return "", false
	}
	if string(b[0x40:0x48]) != btrfsMagic {
		return "", false
	}
	label = string(b[0x12b : 0x12b+256])
	if i := strings.IndexByte(label, 0); i >= 0 {
		label = label[:i]
	}
	return label, true
}

func openBtrfs(r io.ReaderAt, size int64) (*btrfsFS, error) {
	b := &btrfsFS{r: r, cache: map[uint64]*btrfsNode{}, trees: map[uint64]*btrfsTree{}}
	var err error
	// The primary superblock, else its first copy at 64 MiB.
	for _, off := range []int64{btrfsSuperOffset, 64 << 20} {
		if off+btrfsSuperSize > size {
			break
		}
		if err = b.readSuper(off); err == nil {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	if err := b.readSysChunks(); err != nil {
		return nil, err
	}
	if err := b.readChunkTree(); err != nil {
		return nil, err
	}
	root := &btrfsTree{id: 1, bytenr: b.sb.root, level: b.sb.rootLevel}
	b.trees[1] = root
	top, err := b.tree(btrfsFSTree)
	if err != nil {
		return nil, err
	}
	if top == nil {
		return nil, btrfsErr("the top-level subvolume is missing")
	}
	b.top = top
	return b, nil
}

func (b *btrfsFS) readSuper(off int64) error {
	s := make([]byte, btrfsSuperSize)
	if _, err := b.r.ReadAt(s, off); err != nil && err != io.EOF {
		return err
	}
	if string(s[0x40:0x48]) != btrfsMagic {
		return btrfsErr("no superblock at %d", off)
	}
	sb := btrfsSuper{
		root:       binary.LittleEndian.Uint64(s[0x50:]),
		chunkRoot:  binary.LittleEndian.Uint64(s[0x58:]),
		sectorSize: binary.LittleEndian.Uint32(s[0x90:]),
		nodeSize:   binary.LittleEndian.Uint32(s[0x94:]),
		csumType:   binary.LittleEndian.Uint16(s[0xc4:]),
		rootLevel:  s[0xc6],
		chunkLevel: s[0xc7],
		devid:      binary.LittleEndian.Uint64(s[0xc9:]),
	}
	if sb.csumType > 3 {
		return fmt.Errorf("this btrfs file system uses checksum type %d, which cannot be read", sb.csumType)
	}
	if !btrfsSumOK(sb.csumType, s) {
		return btrfsErr("superblock checksum mismatch")
	}
	if devs := binary.LittleEndian.Uint64(s[0x88:]); devs != 1 {
		return fmt.Errorf("this btrfs file system spans %d devices; only file systems on one disk can be browsed", devs)
	}
	incompat := binary.LittleEndian.Uint64(s[0xbc:])
	if incompat&(btrfsExtentTreeV2|btrfsRaidStripeTree) != 0 {
		return fmt.Errorf("this btrfs file system uses features that cannot be browsed yet (incompat flags %#x)", incompat)
	}
	copy(sb.fsid[:], s[0x20:0x30])
	if incompat&btrfsMetadataUUID != 0 {
		copy(sb.fsid[:], s[0x23b:0x24b])
	}
	pow2 := func(v uint32) bool { return v&(v-1) == 0 }
	if sb.sectorSize < 4096 || sb.sectorSize > 65536 || !pow2(sb.sectorSize) ||
		sb.nodeSize < sb.sectorSize || sb.nodeSize > 65536 || !pow2(sb.nodeSize) {
		return btrfsErr("sector size %d, node size %d", sb.sectorSize, sb.nodeSize)
	}
	if sb.rootLevel > btrfsMaxLevel || sb.chunkLevel > btrfsMaxLevel {
		return btrfsErr("tree level")
	}
	n := binary.LittleEndian.Uint32(s[0xa0:])
	if n > 2048 {
		return btrfsErr("system chunk array of %d bytes", n)
	}
	sb.sysChunks = s[0x32b : 0x32b+n]
	b.sb = sb
	return nil
}

// btrfsSumOK checks the checksum at the start of a superblock or tree block.
func btrfsSumOK(typ uint16, b []byte) bool {
	data := b[32:]
	switch typ {
	case 0: // crc32c
		return binary.LittleEndian.Uint32(b) == crc32.Checksum(data, castagnoli)
	case 1: // xxhash64
		return binary.LittleEndian.Uint64(b) == xxhash.Sum64(data)
	case 2:
		sum := sha256.Sum256(data)
		return bytes.Equal(b[:32], sum[:])
	case 3:
		sum := blake2b.Sum256(data)
		return bytes.Equal(b[:32], sum[:])
	}
	return false
}

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// btrfsNameHash is the hash of a name in DIR_ITEM keys: the kernel's
// crc32c(~1, name) without the final inversion.
func btrfsNameHash(name string) uint64 {
	return uint64(^crc32.Update(1, castagnoli, []byte(name)))
}

func (b *btrfsFS) parseChunk(logical uint64, c []byte) (btrfsChunk, error) {
	if len(c) < 48 {
		return btrfsChunk{}, btrfsErr("short chunk item")
	}
	ch := btrfsChunk{logical: logical, length: binary.LittleEndian.Uint64(c)}
	typ := binary.LittleEndian.Uint64(c[24:])
	n := int(binary.LittleEndian.Uint16(c[44:]))
	if n == 0 || len(c) < 48+32*n || ch.length == 0 || logical+ch.length < logical {
		return btrfsChunk{}, btrfsErr("chunk at %d", logical)
	}
	if typ&(btrfsRAID10|btrfsRAID5|btrfsRAID6) != 0 || typ&btrfsRAID0 != 0 && n > 1 {
		return btrfsChunk{}, fmt.Errorf("this btrfs file system stripes data (RAID 0/10/5/6), which cannot be browsed")
	}
	for i := 0; i < n; i++ {
		s := c[48+32*i:]
		if binary.LittleEndian.Uint64(s) == b.sb.devid {
			ch.stripes = append(ch.stripes, binary.LittleEndian.Uint64(s[8:]))
		}
	}
	if len(ch.stripes) == 0 {
		return btrfsChunk{}, btrfsErr("chunk at %d is on another device", logical)
	}
	return ch, nil
}

func (b *btrfsFS) addChunk(ch btrfsChunk) {
	i := sort.Search(len(b.chunks), func(i int) bool { return b.chunks[i].logical >= ch.logical })
	if i < len(b.chunks) && b.chunks[i].logical == ch.logical {
		b.chunks[i] = ch
		return
	}
	b.chunks = append(b.chunks, btrfsChunk{})
	copy(b.chunks[i+1:], b.chunks[i:])
	b.chunks[i] = ch
}

func (b *btrfsFS) readSysChunks() error {
	a := b.sb.sysChunks
	for len(a) > 0 {
		if len(a) < 17+48 {
			return btrfsErr("system chunk array")
		}
		k := readKey(a)
		if k.typ != btrfsChunkItem {
			return btrfsErr("system chunk array item type %d", k.typ)
		}
		n := int(binary.LittleEndian.Uint16(a[17+44:]))
		size := 48 + 32*n
		if len(a) < 17+size {
			return btrfsErr("system chunk array")
		}
		ch, err := b.parseChunk(k.off, a[17:17+size])
		if err != nil {
			return err
		}
		b.addChunk(ch)
		a = a[17+size:]
	}
	return nil
}

func (b *btrfsFS) readChunkTree() error {
	t := btrfsTree{id: 3, bytenr: b.sb.chunkRoot, level: b.sb.chunkLevel}
	return b.each(t, bkey{}, func(k bkey, d []byte) (bool, error) {
		if k.typ == btrfsChunkItem {
			ch, err := b.parseChunk(k.off, d)
			if err != nil {
				return false, err
			}
			b.addChunk(ch)
		}
		return true, nil
	})
}

func (b *btrfsFS) chunkAt(addr uint64) *btrfsChunk {
	i := sort.Search(len(b.chunks), func(i int) bool { return b.chunks[i].logical > addr }) - 1
	if i < 0 || addr-b.chunks[i].logical >= b.chunks[i].length {
		return nil
	}
	return &b.chunks[i]
}

// readAt reads logical bytes from copy m (of the chunk's copies) and fills
// all of p, across chunks.
func (b *btrfsFS) readAt(p []byte, addr uint64, m int) error {
	for len(p) > 0 {
		c := b.chunkAt(addr)
		if c == nil {
			return btrfsErr("no chunk maps address %d", addr)
		}
		n := uint64(len(p))
		if left := c.length - (addr - c.logical); n > left {
			n = left
		}
		phys := c.stripes[m%len(c.stripes)] + (addr - c.logical)
		if phys < c.stripes[m%len(c.stripes)] || phys > math.MaxInt64-n {
			return btrfsErr("address %d", addr)
		}
		got, err := b.r.ReadAt(p[:n], int64(phys)) // #nosec G115 -- checked above
		if uint64(got) < n { // #nosec G115 -- a byte count, not negative
			if err == nil || err == io.EOF {
				err = btrfsErr("address %d lies past the end of the disk", addr)
			}
			return err
		}
		p, addr = p[n:], addr+n
	}
	return nil
}

func (b *btrfsFS) node(addr uint64, level int) (*btrfsNode, error) {
	if level < 0 || level > btrfsMaxLevel {
		return nil, btrfsErr("tree level %d", level)
	}
	if n, ok := b.cache[addr]; ok {
		if int(n.level) != level {
			return nil, btrfsErr("tree block %d has level %d, want %d", addr, n.level, level)
		}
		return n, nil
	}
	c := b.chunkAt(addr)
	if c == nil {
		return nil, btrfsErr("no chunk maps tree block %d", addr)
	}
	size := uint64(b.sb.nodeSize)
	if addr-c.logical+size > c.length {
		return nil, btrfsErr("tree block %d crosses a chunk", addr)
	}
	var err error
	for m := range c.stripes {
		buf := make([]byte, size)
		if err = b.readAt(buf, addr, m); err != nil {
			continue
		}
		var n *btrfsNode
		if n, err = b.parseNode(buf, addr, level); err != nil {
			continue
		}
		if len(b.order) >= btrfsNodeCache {
			delete(b.cache, b.order[0])
			b.order = b.order[1:]
		}
		b.cache[addr] = n
		b.order = append(b.order, addr)
		return n, nil
	}
	return nil, err
}

func (b *btrfsFS) parseNode(buf []byte, addr uint64, level int) (*btrfsNode, error) {
	if !btrfsSumOK(b.sb.csumType, buf) {
		return nil, btrfsErr("checksum of tree block %d", addr)
	}
	if !bytes.Equal(buf[0x20:0x30], b.sb.fsid[:]) || binary.LittleEndian.Uint64(buf[0x30:]) != addr {
		return nil, btrfsErr("tree block %d belongs elsewhere", addr)
	}
	n := &btrfsNode{level: buf[0x64], n: int(binary.LittleEndian.Uint32(buf[0x60:])), b: buf}
	if int(n.level) != level {
		return nil, btrfsErr("tree block %d has level %d, want %d", addr, n.level, level)
	}
	room := len(buf) - btrfsHeaderSize
	if n.level > 0 {
		if n.n == 0 || n.n > room/btrfsKeyPtrSize {
			return nil, btrfsErr("tree block %d has %d pointers", addr, n.n)
		}
		return n, nil
	}
	if n.n > room/btrfsItemSize {
		return nil, btrfsErr("tree block %d has %d items", addr, n.n)
	}
	for i := 0; i < n.n; i++ {
		h := buf[btrfsHeaderSize+i*btrfsItemSize:]
		off, size := uint64(binary.LittleEndian.Uint32(h[17:])), uint64(binary.LittleEndian.Uint32(h[21:]))
		if off+size > uint64(room) { // #nosec G115 -- room is a node size
			return nil, btrfsErr("item %d of tree block %d", i, addr)
		}
	}
	return n, nil
}

// btrfsCursor walks the items of a tree in key order.
type btrfsCursor struct {
	b     *btrfsFS
	nodes []*btrfsNode // root first
	slots []int
	end   bool
}

// seek returns a cursor at the first item with a key >= k.
func (b *btrfsFS) seek(t btrfsTree, k bkey) (*btrfsCursor, error) {
	c := &btrfsCursor{b: b}
	n, err := b.node(t.bytenr, int(t.level))
	if err != nil {
		return nil, err
	}
	for {
		if n.level == 0 {
			i := sort.Search(n.n, func(i int) bool { return !n.key(i).less(k) })
			c.nodes, c.slots = append(c.nodes, n), append(c.slots, i)
			return c, c.settle()
		}
		i := sort.Search(n.n, func(i int) bool { return k.less(n.key(i)) }) - 1
		if i < 0 {
			i = 0
		}
		c.nodes, c.slots = append(c.nodes, n), append(c.slots, i)
		if n, err = b.node(n.ptr(i), int(n.level)-1); err != nil {
			return nil, err
		}
	}
}

// settle moves on from leaves that have no item left.
func (c *btrfsCursor) settle() error {
	d := len(c.nodes) - 1
	for c.slots[d] >= c.nodes[d].n {
		up := d - 1
		for up >= 0 && c.slots[up]+1 >= c.nodes[up].n {
			up--
		}
		if up < 0 {
			c.end = true
			return nil
		}
		c.slots[up]++
		for l := up; l < d; l++ {
			child, err := c.b.node(c.nodes[l].ptr(c.slots[l]), int(c.nodes[l].level)-1)
			if err != nil {
				return err
			}
			c.nodes[l+1], c.slots[l+1] = child, 0
		}
	}
	return nil
}

func (c *btrfsCursor) item() (bkey, []byte, bool) {
	if c.end {
		return bkey{}, nil, false
	}
	leaf, i := c.nodes[len(c.nodes)-1], c.slots[len(c.slots)-1]
	return leaf.key(i), leaf.item(i), true
}

func (c *btrfsCursor) next() error {
	prev, _, ok := c.item()
	if !ok {
		return nil
	}
	c.slots[len(c.slots)-1]++
	if err := c.settle(); err != nil {
		return err
	}
	// Keys only grow; a damaged or crafted tree that repeats blocks
	// would otherwise be walked without end.
	if k, _, ok := c.item(); ok && !prev.less(k) {
		return btrfsErr("tree keys out of order")
	}
	return nil
}

// each calls fn for the items from key k on until fn returns false.
func (b *btrfsFS) each(t btrfsTree, k bkey, fn func(bkey, []byte) (bool, error)) error {
	c, err := b.seek(t, k)
	if err != nil {
		return err
	}
	for {
		k, d, ok := c.item()
		if !ok {
			return nil
		}
		if more, err := fn(k, d); err != nil || !more {
			return err
		}
		if err := c.next(); err != nil {
			return err
		}
	}
}

// find returns the data of the item with exactly key k, or nil.
func (b *btrfsFS) find(t btrfsTree, k bkey) ([]byte, error) {
	c, err := b.seek(t, k)
	if err != nil {
		return nil, err
	}
	if got, d, ok := c.item(); ok && got == k {
		return d, nil
	}
	return nil, nil
}

// tree returns the subvolume tree id, nil when there is none.
func (b *btrfsFS) tree(id uint64) (*btrfsTree, error) {
	if t, ok := b.trees[id]; ok {
		return t, nil
	}
	var item []byte
	err := b.each(*b.trees[1], bkey{obj: id, typ: btrfsRootItem}, func(k bkey, d []byte) (bool, error) {
		if k.obj != id || k.typ != btrfsRootItem {
			return false, nil
		}
		item = d // the last one (highest offset) is current
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	var t *btrfsTree
	if len(item) >= 239 && binary.LittleEndian.Uint32(item[216:]) > 0 { // refs: 0 while it is being deleted
		t = &btrfsTree{id: id, bytenr: binary.LittleEndian.Uint64(item[176:]), level: item[238], dirID: binary.LittleEndian.Uint64(item[168:])}
	}
	b.trees[id] = t
	return t, nil
}

type btrfsInode struct {
	size  uint64
	mode  uint32
	mtime time.Time
}

func (b *btrfsFS) inode(t btrfsTree, ino uint64) (btrfsInode, error) {
	d, err := b.find(t, bkey{obj: ino, typ: btrfsInodeItem})
	if err != nil {
		return btrfsInode{}, err
	}
	if len(d) < 160 {
		return btrfsInode{}, btrfsErr("inode %d of subvolume %d is missing", ino, t.id)
	}
	sec := int64(binary.LittleEndian.Uint64(d[136:])) // #nosec G115 -- a time stamp
	return btrfsInode{
		size:  binary.LittleEndian.Uint64(d[16:]),
		mode:  binary.LittleEndian.Uint32(d[52:]),
		mtime: time.Unix(sec, int64(binary.LittleEndian.Uint32(d[144:]))),
	}, nil
}

// btrfsObj is a file or folder: an inode in a subvolume tree. empty marks
// the stand-in folder of a subvolume that is linked but not reachable, as
// in a snapshot of a subvolume that had subvolumes inside (the kernel
// shows an empty folder there too).
type btrfsObj struct {
	tree  btrfsTree
	ino   uint64
	empty bool
	in    btrfsInode
}

func (o btrfsObj) id() uint64 {
	h := fnv.New64a()
	var b [17]byte
	binary.LittleEndian.PutUint64(b[:], o.tree.id)
	binary.LittleEndian.PutUint64(b[8:], o.ino)
	if o.empty {
		b[16] = 1
	}
	_, _ = h.Write(b[:])
	if id := h.Sum64(); id != 0 {
		return id
	}
	return 1
}

func (o btrfsObj) isDir() bool { return o.empty || o.in.mode&0o170000 == 0o040000 }

func (b *btrfsFS) obj(t btrfsTree, ino uint64) (btrfsObj, error) {
	in, err := b.inode(t, ino)
	return btrfsObj{tree: t, ino: ino, in: in}, err
}

type btrfsDirEnt struct {
	name string
	loc  bkey
	typ  uint8
}

// parseDirItems reads the entries of a DIR_ITEM or DIR_INDEX item.
func parseDirItems(d []byte) ([]btrfsDirEnt, error) {
	var out []btrfsDirEnt
	for len(d) > 0 {
		if len(d) < 30 {
			return nil, btrfsErr("short directory item")
		}
		dataLen, nameLen := int(binary.LittleEndian.Uint16(d[25:])), int(binary.LittleEndian.Uint16(d[27:]))
		if len(d) < 30+nameLen+dataLen {
			return nil, btrfsErr("short directory item")
		}
		out = append(out, btrfsDirEnt{name: string(d[30 : 30+nameLen]), loc: readKey(d), typ: d[29]})
		d = d[30+nameLen+dataLen:]
	}
	return out, nil
}

// follow returns what a directory entry of folder dir points to.
func (b *btrfsFS) follow(dir btrfsObj, e btrfsDirEnt) (btrfsObj, error) {
	switch e.loc.typ {
	case btrfsInodeItem:
		return b.obj(dir.tree, e.loc.obj)
	case btrfsRootItem:
		// A subvolume: only entered when the root tree links it here.
		ref, err := b.find(*b.trees[1], bkey{obj: dir.tree.id, typ: btrfsRootRef, off: e.loc.obj})
		if err != nil {
			return btrfsObj{}, err
		}
		stub := btrfsObj{tree: dir.tree, ino: e.loc.obj, empty: true, in: btrfsInode{mode: 0o040755, mtime: dir.in.mtime}}
		if len(ref) < 18 || binary.LittleEndian.Uint64(ref) != dir.ino {
			return stub, nil
		}
		if n := int(binary.LittleEndian.Uint16(ref[16:])); len(ref) < 18+n || string(ref[18:18+n]) != e.name {
			return stub, nil
		}
		t, err := b.tree(e.loc.obj)
		if err != nil || t == nil {
			return stub, err
		}
		return b.obj(*t, t.dirID)
	}
	return btrfsObj{}, btrfsErr("directory entry %q points to item type %d", e.name, e.loc.typ)
}

func (b *btrfsFS) lookup(dir btrfsObj, name string) (btrfsObj, error) {
	if dir.empty {
		return btrfsObj{}, fs.ErrNotExist
	}
	d, err := b.find(dir.tree, bkey{obj: dir.ino, typ: btrfsDirItem, off: btrfsNameHash(name)})
	if err != nil {
		return btrfsObj{}, err
	}
	es, err := parseDirItems(d)
	if err != nil {
		return btrfsObj{}, err
	}
	for _, e := range es {
		if e.name == name {
			return b.follow(dir, e)
		}
	}
	return btrfsObj{}, fs.ErrNotExist
}

func (b *btrfsFS) rootObj() (btrfsObj, error) { return b.obj(*b.top, b.top.dirID) }

// walk finds a path (io/fs form) without following symbolic links.
func (b *btrfsFS) walk(op, name string) (btrfsObj, error) {
	if !fs.ValidPath(name) {
		return btrfsObj{}, &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}
	o, err := b.rootObj()
	if err != nil || name == "." {
		return o, err
	}
	for _, part := range strings.Split(name, "/") {
		if !o.isDir() {
			return btrfsObj{}, &fs.PathError{Op: op, Path: name, Err: errors.New("not a directory")}
		}
		if o, err = b.lookup(o, part); err != nil {
			return btrfsObj{}, &fs.PathError{Op: op, Path: name, Err: err}
		}
	}
	return o, nil
}

func (b *btrfsFS) readDir(dir btrfsObj) ([]btrfsDirEnt, error) {
	if dir.empty {
		return nil, nil
	}
	var out []btrfsDirEnt
	err := b.each(dir.tree, bkey{obj: dir.ino, typ: btrfsDirIndex}, func(k bkey, d []byte) (bool, error) {
		if k.obj != dir.ino || k.typ != btrfsDirIndex {
			return false, nil
		}
		es, err := parseDirItems(d)
		if err != nil || len(es) == 0 {
			return false, err
		}
		out = append(out, es[0])
		return true, nil
	})
	return out, err
}

// Open, ReadDir, Stat and ReadLink make the reader an io/fs file system
// for ioFS (lockedFS serialises the calls).

func (b *btrfsFS) Open(name string) (fs.File, error) {
	o, err := b.walk("open", name)
	if err != nil {
		return nil, err
	}
	info := btrfsInfo{name: path.Base(name), o: o}
	if o.isDir() {
		return &btrfsDirFile{b: b, info: info}, nil
	}
	return &btrfsFile{b: b, info: info, size: info.Size()}, nil
}

func (b *btrfsFS) Stat(name string) (fs.FileInfo, error) {
	o, err := b.walk("stat", name)
	if err != nil {
		return nil, err
	}
	return btrfsInfo{name: path.Base(name), o: o}, nil
}

func (b *btrfsFS) ReadDir(name string) ([]fs.DirEntry, error) {
	o, err := b.walk("readdir", name)
	if err != nil {
		return nil, err
	}
	if !o.isDir() {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: errors.New("not a directory")}
	}
	es, err := b.readDir(o)
	if err != nil {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: err}
	}
	out := make([]fs.DirEntry, 0, len(es))
	for _, e := range es {
		out = append(out, &btrfsDirEntry{b: b, dir: o, e: e})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

// maxLinkSize bounds a symbolic link target (PATH_MAX).
const maxLinkSize = 4096

func (b *btrfsFS) ReadLink(name string) (string, error) {
	o, err := b.walk("readlink", name)
	if err != nil {
		return "", err
	}
	if o.in.mode&0o170000 != 0o120000 {
		return "", &fs.PathError{Op: "readlink", Path: name, Err: fs.ErrInvalid}
	}
	f := &btrfsFile{b: b, info: btrfsInfo{name: path.Base(name), o: o}}
	f.size = min(f.info.Size(), maxLinkSize)
	t, err := io.ReadAll(f)
	if err != nil {
		return "", &fs.PathError{Op: "readlink", Path: name, Err: err}
	}
	return string(t), nil
}

type btrfsInfo struct {
	name string
	o    btrfsObj
}

func (i btrfsInfo) Name() string { return i.name }
func (i btrfsInfo) Size() int64 {
	if i.o.in.size > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(i.o.in.size)
}
func (i btrfsInfo) ModTime() time.Time { return i.o.in.mtime }
func (i btrfsInfo) IsDir() bool        { return i.o.isDir() }
func (i btrfsInfo) Sys() any           { return i.o.id() }
func (i btrfsInfo) Mode() fs.FileMode {
	m := fs.FileMode(i.o.in.mode & 0o777)
	switch i.o.in.mode & 0o170000 {
	case 0o040000:
		m |= fs.ModeDir
	case 0o120000:
		m |= fs.ModeSymlink
	case 0o020000:
		m |= fs.ModeDevice | fs.ModeCharDevice
	case 0o060000:
		m |= fs.ModeDevice
	case 0o010000:
		m |= fs.ModeNamedPipe
	case 0o140000:
		m |= fs.ModeSocket
	}
	if i.o.empty {
		m |= fs.ModeDir
	}
	return m
}

type btrfsDirEntry struct {
	b   *btrfsFS
	dir btrfsObj
	e   btrfsDirEnt
}

func (d *btrfsDirEntry) Name() string { return d.e.name }
func (d *btrfsDirEntry) IsDir() bool {
	return d.e.typ == 2 || d.e.loc.typ == btrfsRootItem
}
func (d *btrfsDirEntry) Type() fs.FileMode {
	switch {
	case d.IsDir():
		return fs.ModeDir
	case d.e.typ == 7:
		return fs.ModeSymlink
	}
	return 0
}
func (d *btrfsDirEntry) Info() (fs.FileInfo, error) {
	o, err := d.b.follow(d.dir, d.e)
	if err != nil {
		return nil, err
	}
	return btrfsInfo{name: d.e.name, o: o}, nil
}

type btrfsDirFile struct {
	b    *btrfsFS
	info btrfsInfo
	ents []fs.DirEntry
	read bool
}

func (d *btrfsDirFile) Stat() (fs.FileInfo, error) { return d.info, nil }
func (d *btrfsDirFile) Close() error               { return nil }
func (d *btrfsDirFile) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.info.name, Err: errors.New("is a directory")}
}

func (d *btrfsDirFile) ReadDir(n int) ([]fs.DirEntry, error) {
	if !d.read {
		es, err := d.b.readDir(d.info.o)
		if err != nil {
			return nil, err
		}
		for _, e := range es {
			d.ents = append(d.ents, &btrfsDirEntry{b: d.b, dir: d.info.o, e: e})
		}
		d.read = true
	}
	if n <= 0 {
		out := d.ents
		d.ents = nil
		return out, nil
	}
	if len(d.ents) == 0 {
		return nil, io.EOF
	}
	n = min(n, len(d.ents))
	out := d.ents[:n]
	d.ents = d.ents[n:]
	return out, nil
}

// btrfsExtent is one EXTENT_DATA item of a file.
type btrfsExtent struct {
	start, length int64 // the part of the file it covers
	kind          uint8 // 0 inline, 1 regular, 2 preallocated
	comp          uint8
	inline        []byte
	ram           uint64
	addr, disk    uint64 // the extent on disk (0: a hole)
	off           uint64 // where the file's part starts in the (decompressed) extent
}

func parseExtent(k bkey, d []byte) (btrfsExtent, error) {
	if len(d) < 21 || k.off > math.MaxInt64 {
		return btrfsExtent{}, btrfsErr("short file extent")
	}
	e := btrfsExtent{start: int64(k.off), ram: binary.LittleEndian.Uint64(d[8:]), comp: d[16], kind: d[20]}
	if d[17] != 0 || binary.LittleEndian.Uint16(d[18:]) != 0 {
		return btrfsExtent{}, errors.New("this file is encrypted or encoded in a way that cannot be read")
	}
	if e.comp > 3 {
		return btrfsExtent{}, fmt.Errorf("this file uses compression type %d, which cannot be read", e.comp)
	}
	var n uint64
	switch e.kind {
	case 0:
		e.inline = d[21:]
		n = e.ram
		if e.comp == 0 {
			n = uint64(len(e.inline))
		}
		if n > btrfsMaxInline {
			return btrfsExtent{}, btrfsErr("inline extent of %d bytes", n)
		}
	case 1, 2:
		if len(d) < 53 {
			return btrfsExtent{}, btrfsErr("short file extent")
		}
		e.addr, e.disk = binary.LittleEndian.Uint64(d[21:]), binary.LittleEndian.Uint64(d[29:])
		e.off, n = binary.LittleEndian.Uint64(d[37:]), binary.LittleEndian.Uint64(d[45:])
		if e.comp != 0 && e.addr != 0 && (e.ram > btrfsMaxCompressed || e.disk > btrfsMaxCompressed) {
			return btrfsExtent{}, btrfsErr("compressed extent of %d bytes", e.ram)
		}
	default:
		return btrfsExtent{}, btrfsErr("file extent type %d", e.kind)
	}
	if n > uint64(math.MaxInt64-e.start) { // #nosec G115 -- start is not negative
		n = uint64(math.MaxInt64 - e.start) // #nosec G115 -- start is not negative
	}
	e.length = int64(n) // #nosec G115 -- bounded above
	return e, nil
}

// btrfsFile reads a file front to back through its extents.
type btrfsFile struct {
	b       *btrfsFS
	info    btrfsInfo
	size    int64
	pos     int64
	cur     *btrfsCursor
	ext     *btrfsExtent // the current or next extent; nil: no more
	started bool
}

func (f *btrfsFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *btrfsFile) Close() error               { return nil }

// load takes the next extent of the file from the cursor.
func (f *btrfsFile) load() error {
	f.ext = nil
	k, d, ok := f.cur.item()
	if !ok || k.obj != f.info.o.ino || k.typ != btrfsExtentData {
		return nil
	}
	e, err := parseExtent(k, d)
	if err != nil {
		return err
	}
	f.ext = &e
	return f.cur.next()
}

func (f *btrfsFile) Read(p []byte) (int, error) {
	if f.pos >= f.size {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	o := f.info.o
	if !f.started {
		f.started = true
		if o.empty || o.isDir() {
			f.size = 0
			return 0, io.EOF
		}
		c, err := f.b.seek(o.tree, bkey{obj: o.ino, typ: btrfsExtentData})
		if err != nil {
			return 0, err
		}
		f.cur = c
		if err := f.load(); err != nil {
			return 0, err
		}
	}
	for f.ext != nil && f.ext.start+f.ext.length <= f.pos {
		if err := f.load(); err != nil {
			return 0, err
		}
	}
	want := min(int64(len(p)), f.size-f.pos)
	if f.ext == nil || f.ext.start > f.pos { // a hole up to the next extent
		if f.ext != nil {
			want = min(want, f.ext.start-f.pos)
		}
		clear(p[:want])
		f.pos += want
		return int(want), nil
	}
	want = min(want, f.ext.start+f.ext.length-f.pos)
	if err := f.b.readExtent(p[:want], f.ext, f.pos-f.ext.start); err != nil {
		return 0, err
	}
	f.pos += want
	return int(want), nil
}

// readExtent fills p with the extent's bytes from rel on.
func (b *btrfsFS) readExtent(p []byte, e *btrfsExtent, rel int64) error {
	switch {
	case e.kind == 0:
		data := e.inline
		if e.comp != 0 {
			var err error
			if data, err = btrfsDecompress(e.comp, e.inline, int(e.ram), int(b.sb.sectorSize)); err != nil { // #nosec G115 -- bounded in parseExtent
				return err
			}
		}
		copyZero(p, data, rel)
		return nil
	case e.kind == 2 || e.addr == 0:
		clear(p)
		return nil
	case e.comp == 0:
		addr := e.addr + e.off + uint64(rel) // #nosec G115 -- rel is not negative
		if addr < e.addr {
			return btrfsErr("extent address")
		}
		var err error
		for m := 0; m < b.copies(addr); m++ { // a DUP or mirror copy when one cannot be read
			if err = b.readAt(p, addr, m); err == nil {
				return nil
			}
		}
		return err
	}
	key := [3]uint64{e.addr, e.disk, uint64(e.comp)}
	if b.extKey != key || b.extData == nil {
		var err error
		for m := 0; m < b.copies(e.addr); m++ {
			raw := make([]byte, e.disk)
			if err = b.readAt(raw, e.addr, m); err != nil {
				continue
			}
			var data []byte
			if data, err = btrfsDecompress(e.comp, raw, int(e.ram), int(b.sb.sectorSize)); err == nil { // #nosec G115 -- bounded in parseExtent
				b.extKey, b.extData = key, data
				break
			}
		}
		if err != nil {
			return err
		}
	}
	if e.off > uint64(len(b.extData)) {
		clear(p)
		return nil
	}
	copyZero(p, b.extData[e.off:], rel)
	return nil
}

// copies is how many copies the chunk at addr keeps on this device.
func (b *btrfsFS) copies(addr uint64) int {
	if c := b.chunkAt(addr); c != nil {
		return len(c.stripes)
	}
	return 1
}

// copyZero copies src[from:] into p and zeroes what src does not cover.
func copyZero(p, src []byte, from int64) {
	n := 0
	if from < int64(len(src)) {
		n = copy(p, src[from:])
	}
	clear(p[n:])
}
