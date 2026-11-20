package vmfs

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/masahiro331/go-ext4-filesystem/ext4"
	"github.com/masahiro331/go-xfs-filesystem/xfs"
)

// inodeFS is a file system like the ext4 and XFS readers: paths are looked
// up name by name from the root inode, so a directory entry that points
// back to its own folder (or an ancestor) makes an endless tree.
type inodeFS map[uint64]inodeNode

type inodeNode struct {
	children map[string]uint64 // nil: a file
	data     string
}

type inodeInfo struct {
	name string
	ino  uint64
	node inodeNode
}

func (i inodeInfo) Name() string       { return i.name }
func (i inodeInfo) Size() int64        { return int64(len(i.node.data)) }
func (i inodeInfo) ModTime() time.Time { return time.Time{} }
func (i inodeInfo) IsDir() bool        { return i.node.children != nil }
func (i inodeInfo) Sys() any           { return i.ino }
func (i inodeInfo) Mode() fs.FileMode {
	if i.IsDir() {
		return fs.ModeDir | 0o755
	}
	return 0o644
}

func (f inodeFS) lookup(name string) (inodeInfo, error) {
	cur := inodeInfo{name: ".", ino: 2, node: f[2]}
	if name == "." {
		return cur, nil
	}
	for _, part := range strings.Split(name, "/") {
		ino, ok := cur.node.children[part]
		if !ok {
			return inodeInfo{}, fs.ErrNotExist
		}
		cur = inodeInfo{name: part, ino: ino, node: f[ino]}
	}
	return cur, nil
}

func (f inodeFS) Stat(name string) (fs.FileInfo, error) { return f.lookup(name) }

func (f inodeFS) ReadDir(name string) ([]fs.DirEntry, error) {
	d, err := f.lookup(name)
	if err != nil || !d.IsDir() {
		return nil, fs.ErrInvalid
	}
	var out []fs.DirEntry
	for n, ino := range d.node.children {
		out = append(out, fs.FileInfoToDirEntry(inodeInfo{name: n, ino: ino, node: f[ino]}))
	}
	return out, nil
}

func (f inodeFS) Open(name string) (fs.File, error) {
	e, err := f.lookup(name)
	if err != nil || e.IsDir() {
		return nil, fs.ErrInvalid
	}
	return fstest.MapFS{"f": {Data: []byte(e.node.data)}}.Open("f")
}

// P6 (0.55 review): a folder that contains itself, twice, gave 2^256 walks
// within the depth limit and a restore that created folders until it was
// cancelled. Each folder is now entered once; the loops are reported.
func TestExtractDirectoryLoop(t *testing.T) {
	f := &ioFS{fsys: inodeFS{
		2:  {children: map[string]uint64{"a": 10}},
		10: {children: map[string]uint64{"f.txt": 11, "self1": 10, "self2": 10, "up": 2, "b1": 12, "b2": 12}},
		11: {data: "data"},
		// One folder under two names: a crafted "hard link" to a folder,
		// which nests into 2^n paths without any loop.
		12: {children: map[string]uint64{"g.txt": 13}},
		13: {data: "more"},
	}}
	dir := t.TempDir()
	done := make(chan struct{})
	var st *ExtractStats
	var err error
	go func() {
		defer close(done)
		st, err = Extract(context.Background(), f, []string{"/a"}, dir)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the walk does not end")
	}
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "a", "f.txt")); err != nil || string(b) != "data" {
		t.Fatalf("a/f.txt: %q %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "a", "b1", "g.txt")); err != nil || string(b) != "more" {
		t.Fatalf("a/b1/g.txt: %q %v", b, err)
	}
	// Copied: a, a/b1 and a/up (the root, not walked before); skipped:
	// a/self1, a/self2, a/b2 (b1 was copied) and a/up/a (a itself).
	if st.Dirs != 3 || st.Files != 2 || len(st.Errors) != 4 {
		t.Fatalf("%d folders, %d files, errors %q", st.Dirs, st.Files, st.Errors)
	}
	for _, e := range st.Errors {
		if !strings.Contains(e, "loop") {
			t.Errorf("unexpected error %q", e)
		}
	}
	for _, p := range []string{"self1", "self2", "b2"} {
		if _, err := os.Stat(filepath.Join(dir, "a", p)); err == nil {
			t.Errorf("a/%s was created", p)
		}
	}
	// Selected paths inside each other are each copied, as before.
	st, err = Extract(context.Background(), f, []string{"/a/b1", "/a"}, t.TempDir())
	if err != nil || st.Files != 3 {
		t.Fatalf("overlapping selection: %d files, %v %q", st.Files, err, st.Errors)
	}
}

// The ext4 and XFS readers keep the inode in unexported fields that fileID
// reads by name: an update of the readers that renames them must fail here
// rather than silently drop the loop protection.
func TestFileIDFields(t *testing.T) {
	e := reflect.TypeOf(ext4.FileInfo{})
	if f, ok := e.FieldByName("ino"); !ok || f.Type.Kind() != reflect.Int64 {
		t.Errorf("ext4.FileInfo has no ino field: %v", f)
	}
	x := reflect.TypeOf(xfs.FileInfo{})
	f, ok := x.FieldByName("inode")
	if !ok || f.Type.Kind() != reflect.Pointer {
		t.Fatalf("xfs.FileInfo has no inode pointer: %v", f)
	}
	if c, ok := f.Type.Elem().FieldByName("inodeCore"); !ok || c.Type.Kind() != reflect.Struct {
		t.Errorf("xfs.Inode has no inodeCore: %v", c)
	}
}

// fileID on look-alikes of the reader types (same field names).
type fakeExt4Info struct {
	inodeInfo
	ino int64
}

func (fakeExt4Info) Sys() any { return nil }

type fakeXFSCore struct {
	Mtime uint64
	Gen   uint32
	UUID  [4]byte
}

type fakeXFSInode struct{ inodeCore fakeXFSCore }

type fakeXFSInfo struct {
	inodeInfo
	inode *fakeXFSInode
}

func (fakeXFSInfo) Sys() any { return nil }

func TestFileID(t *testing.T) {
	if id := fileID(fakeExt4Info{ino: 12}); id != 12 {
		t.Errorf("ext4-like: %d", id)
	}
	a := &fakeXFSInode{fakeXFSCore{Mtime: 1, Gen: 7}}
	a2 := &fakeXFSInode{fakeXFSCore{Mtime: 1, Gen: 7}} // the same inode read again
	b := &fakeXFSInode{fakeXFSCore{Mtime: 2, Gen: 7}}
	ida, ida2, idb := fileID(fakeXFSInfo{inode: a}), fileID(fakeXFSInfo{inode: a2}), fileID(fakeXFSInfo{inode: b})
	if ida == 0 || ida != ida2 || ida == idb {
		t.Errorf("XFS-like: %d %d %d", ida, ida2, idb)
	}
	if id := fileID(fakeXFSInfo{}); id != 0 {
		t.Errorf("no inode: %d", id)
	}
	if id := fileID(inodeInfo{ino: 5}); id != 5 {
		t.Errorf("Sys: %d", id)
	}
	if id := fileID(linkInfo{name: "x"}); id != 0 {
		t.Errorf("unknown: %d", id)
	}
}
