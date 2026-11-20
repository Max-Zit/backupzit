package imaging

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/max-zit/backupzit/internal/repo"
	ntfs "www.velocidex.com/golang/go-ntfs/parser"
)

// PartitionReader exposes a partition image as an io.ReaderAt. Blocks are
// loaded from the repository on demand and cached; unused blocks read as
// zeros.
type PartitionReader struct {
	ctx  context.Context
	r    *repo.Repository
	p    *repo.PartitionImage
	ids  []repo.ID
	size int64

	mu    sync.Mutex
	cache map[uint64]*list.Element
	lru   *list.List
	max   int
}

type cachedBlock struct {
	idx  uint64
	data []byte
}

// NewPartitionReader opens the block map of an included partition.
func NewPartitionReader(ctx context.Context, r *repo.Repository, p *repo.PartitionImage) (*PartitionReader, error) {
	if !p.Included {
		return nil, fmt.Errorf("partition %d was not included in the backup", p.Number)
	}
	ids, err := r.LoadBlockMap(ctx, p)
	if err != nil {
		return nil, err
	}
	return &PartitionReader{ctx: ctx, r: r, p: p, ids: ids, size: int64(p.Length), // #nosec G115 -- disk offsets, sizes and partition numbers are within range
		cache: map[uint64]*list.Element{}, lru: list.New(), max: 64}, nil
}

// Size is the partition length in bytes.
func (pr *PartitionReader) Size() int64 { return pr.size }

func (pr *PartitionReader) block(idx uint64) ([]byte, error) {
	pr.mu.Lock()
	if e, ok := pr.cache[idx]; ok {
		pr.lru.MoveToFront(e)
		data := e.Value.(*cachedBlock).data
		pr.mu.Unlock()
		return data, nil
	}
	pr.mu.Unlock()

	id := pr.ids[idx]
	var data []byte
	if !id.IsNull() {
		var err error
		data, err = pr.r.LoadBlob(pr.ctx, repo.DataBlob, id)
		if err != nil {
			return nil, fmt.Errorf("block %d: %w", idx, err)
		}
	}
	pr.mu.Lock()
	defer pr.mu.Unlock()
	pr.cache[idx] = pr.lru.PushFront(&cachedBlock{idx: idx, data: data})
	for pr.lru.Len() > pr.max {
		old := pr.lru.Back()
		pr.lru.Remove(old)
		delete(pr.cache, old.Value.(*cachedBlock).idx)
	}
	return data, nil
}

// ReadAt implements io.ReaderAt.
func (pr *PartitionReader) ReadAt(b []byte, off int64) (int, error) {
	if off >= pr.size {
		return 0, io.EOF
	}
	bs := int64(pr.p.BlockSize)
	n := 0
	for n < len(b) && off < pr.size {
		idx := uint64(off / bs) // #nosec G115 -- disk offsets, sizes and partition numbers are within range
		inner := off % bs
		data, err := pr.block(idx)
		if err != nil {
			return n, err
		}
		want := min(int64(len(b)-n), bs-inner, pr.size-off)
		if data == nil { // unused block
			clear(b[n : n+int(want)])
		} else {
			avail := int64(len(data)) - inner
			if avail < want {
				clear(b[n+int(max(avail, 0)) : n+int(want)])
				want2 := max(avail, 0)
				copy(b[n:], data[inner:inner+want2])
			} else {
				copy(b[n:], data[inner:inner+want])
			}
		}
		n += int(want)
		off += want
	}
	if n < len(b) {
		return n, io.EOF
	}
	return n, nil
}

// Volume is a read-only NTFS file system inside a partition image.
type Volume struct {
	ctx *ntfs.NTFSContext
	mu  sync.Mutex // go-ntfs contexts are not safe for concurrent use
}

// Entry is a directory entry inside an image.
type Entry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"` // `\Users\ana`
	IsDir   bool      `json:"is_dir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
}

// ErrNotNTFS is returned for partitions without an NTFS file system.
var ErrNotNTFS = errors.New("partition does not contain an NTFS file system")

// OpenVolume opens the NTFS file system of an included partition.
func OpenVolume(ctx context.Context, r *repo.Repository, p *repo.PartitionImage) (*Volume, error) {
	if !strings.EqualFold(p.FileSystem, "NTFS") {
		return nil, ErrNotNTFS
	}
	pr, err := NewPartitionReader(ctx, r, p)
	if err != nil {
		return nil, err
	}
	return OpenNTFS(pr)
}

// OpenNTFS opens an NTFS file system on any reader (e.g. a partition inside
// a virtual machine disk).
func OpenNTFS(pr io.ReaderAt) (*Volume, error) {
	paged, err := ntfs.NewPagedReader(pr, 1024*1024, 128)
	if err != nil {
		return nil, err
	}
	c, err := ntfs.GetNTFSContext(paged, 0)
	if err != nil {
		return nil, fmt.Errorf("read NTFS: %w", err)
	}
	return &Volume{ctx: c}, nil
}

// CleanPath normalises a path inside the volume to `\a\b` form.
func CleanPath(p string) string {
	p = strings.ReplaceAll(p, "/", `\`)
	var parts []string
	for _, s := range strings.Split(p, `\`) {
		switch s {
		case "", ".":
		case "..":
			if len(parts) > 0 {
				parts = parts[:len(parts)-1]
			}
		default:
			parts = append(parts, s)
		}
	}
	return `\` + strings.Join(parts, `\`)
}

func (v *Volume) open(p string) (*ntfs.MFT_ENTRY, error) {
	root, err := v.ctx.GetMFT(5)
	if err != nil {
		return nil, err
	}
	p = CleanPath(p)
	if p == `\` {
		return root, nil
	}
	return root.Open(v.ctx, strings.TrimPrefix(p, `\`))
}

// List returns the entries of a directory, folders first. NTFS metadata
// files ($MFT, ...) are hidden. Every name of a file with hard links is an
// entry of its own, and every named data stream (alternate data stream) is
// listed as "name:stream".
func (v *Volume) List(dir string) ([]Entry, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	dir = CleanPath(dir)
	e, err := v.open(dir)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", dir, err)
	}
	// The names come from the directory's index, not from ntfs.ListDir: it
	// lists each file once with one of its names (from any directory), so
	// further hard links were missing or showed a name of another folder.
	dirRef := uint64(e.Record_number())
	stats := map[uint64][]*ntfs.FileInfo{}
	seen := map[string]bool{}
	var out []Entry
	for _, idx := range e.Dir(v.ctx) {
		fn := idx.File()
		name := fn.Name()
		if !SafeName(name) || fn.NameType().Name == "DOS" {
			continue
		}
		if strings.HasPrefix(name, "$") && dir == `\` {
			continue
		}
		key := strings.ToLower(name)
		if seen[key] {
			continue
		}
		ref := idx.MftReference()
		infos, ok := stats[ref]
		if !ok {
			// Index blocks no longer in use may still hold entries of
			// deleted files: only names the file itself has in this
			// directory count.
			if mft, err := v.ctx.GetMFT(int64(ref)); err == nil && hasFileName(v.ctx, mft, name, dirRef) { // #nosec G115 -- MFT references are 48 bit
				infos = ntfs.Stat(v.ctx, mft)
			}
			if len(infos) == 0 {
				continue
			}
			stats[ref] = infos
		} else if mft, err := v.ctx.GetMFT(int64(ref)); err != nil || !hasFileName(v.ctx, mft, name, dirRef) { // #nosec G115 -- MFT references are 48 bit
			continue
		}
		seen[key] = true
		p := dir + `\` + name
		if dir == `\` {
			p = `\` + name
		}
		// ntfs.Stat returns the file's data streams under one of its
		// names; only the stream part (":stream") is taken from it.
		streams := map[string]bool{}
		for _, fi := range infos {
			if fi == nil || fi.IsSlack {
				continue
			}
			stream := ""
			if i := strings.IndexByte(fi.Name, ':'); i >= 0 {
				stream = fi.Name[i:]
			}
			// A stream name comes from the disk too: one with a slash,
			// backslash or NUL (a crafted volume) would leave the folder.
			if streams[stream] || !SafeName(name+stream) {
				continue
			}
			streams[stream] = true
			out = append(out, Entry{Name: name + stream, Path: p + stream, IsDir: fi.IsDir && stream == "", Size: fi.Size, ModTime: fi.Mtime})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// hasFileName reports whether mft has the $FILE_NAME name in the directory
// with MFT record parent.
func hasFileName(c *ntfs.NTFSContext, mft *ntfs.MFT_ENTRY, name string, parent uint64) bool {
	for _, attr := range mft.EnumerateAttributes(c) {
		if attr.Type().Value != ntfs.ATTR_TYPE_FILE_NAME {
			continue
		}
		fn := c.Profile.FILE_NAME(attr.Data(c), 0)
		if fn.MftReference() == parent && fn.Name() == name {
			return true
		}
	}
	return false
}

// StreamName returns the name of the alternate data stream a path inside
// the volume names (`\dir\file:stream`), "" for the file's main data.
func StreamName(p string) string {
	base := p[strings.LastIndexAny(p, `\/`)+1:]
	if i := strings.IndexByte(base, ':'); i >= 0 {
		return base[i+1:]
	}
	return ""
}

// openData opens the data of a file, or of its alternate data stream when
// the path ends in ":stream". (Opening the path alone gives the file:
// go-ntfs drops the stream part.)
func (v *Volume) openData(p string) (ntfs.RangeReaderAt, error) {
	mft, err := v.open(p)
	if err != nil {
		return nil, err
	}
	stream := ntfs.WILDCARD_STREAM_NAME
	if s := StreamName(p); s != "" {
		stream = s
	}
	return ntfs.OpenStream(v.ctx, mft, ntfs.ATTR_TYPE_DATA, ntfs.WILDCARD_STREAM_ID, stream)
}

// Stat returns the entry for a path.
func (v *Volume) Stat(p string) (Entry, error) {
	p = CleanPath(p)
	if p == `\` {
		return Entry{Name: `\`, Path: `\`, IsDir: true}, nil
	}
	parent := CleanPath(p[:strings.LastIndex(p, `\`)])
	name := p[strings.LastIndex(p, `\`)+1:]
	entries, err := v.List(parent)
	if err != nil {
		return Entry{}, err
	}
	for _, e := range entries {
		if strings.EqualFold(e.Name, name) {
			return e, nil
		}
	}
	return Entry{}, fmt.Errorf("%s: not found", p)
}

// ExtractStats summarises an extraction.
type ExtractStats struct {
	Files    uint64
	Dirs     uint64
	Bytes    uint64
	Duration time.Duration
	Errors   []string
}

// Extract copies paths (files or folders, recursively) out of the volume.
// Each path is written to dest(path), e.g. `D:\Restore\Users\ana\x.txt`.
func (v *Volume) Extract(ctx context.Context, paths []string, dest func(p string) string, progress func(*ExtractStats)) (*ExtractStats, error) {
	st := &ExtractStats{}
	start := time.Now()
	var walk func(e Entry, depth int) error
	walk = func(e Entry, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth > MaxDepth {
			st.Errors = append(st.Errors, fmt.Sprintf("%s: more than %d folders deep (damaged file system?), skipped", e.Path, MaxDepth))
			return nil
		}
		target := dest(e.Path)
		if e.IsDir {
			if err := os.MkdirAll(target, 0o755); err != nil { // #nosec G301 -- folder of restored or copied files, like the original
				st.Errors = append(st.Errors, fmt.Sprintf("%s: %v", e.Path, err))
				return nil
			}
			children, err := v.List(e.Path)
			if err != nil {
				st.Errors = append(st.Errors, fmt.Sprintf("%s: %v", e.Path, err))
				return nil
			}
			for _, c := range children {
				if err := walk(c, depth+1); err != nil {
					return err
				}
			}
			st.Dirs++
			if !e.ModTime.IsZero() {
				_ = os.Chtimes(target, e.ModTime, e.ModTime)
			}
			return nil
		}
		n, err := v.extractFile(e, target)
		if err != nil {
			st.Errors = append(st.Errors, fmt.Sprintf("%s: %v", e.Path, err))
			return nil
		}
		st.Files++
		st.Bytes += uint64(n) // #nosec G115 -- disk offsets, sizes and partition numbers are within range
		if progress != nil {
			progress(st)
		}
		return nil
	}
	for _, p := range paths {
		e, err := v.Stat(p)
		if err != nil {
			st.Errors = append(st.Errors, err.Error())
			continue
		}
		if err := walk(e, 0); err != nil {
			return st, err
		}
	}
	st.Duration = time.Since(start)
	return st, nil
}

func (v *Volume) extractFile(e Entry, target string) (int64, error) {
	v.mu.Lock()
	data, err := v.openData(e.Path)
	v.mu.Unlock()
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil { // #nosec G301 -- folder of restored or copied files, like the original
		return 0, err
	}
	tmp := TempTarget(target)
	// NTFS has no Unix permissions: only the owner may read restored
	// files on Linux (on Windows the target folder's ACL applies).
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) // #nosec G304 -- file in the restore target chosen by the administrator
	if err != nil {
		return 0, err
	}
	buf := make([]byte, 1<<20)
	var off int64
	for off < e.Size {
		want := min(int64(len(buf)), e.Size-off)
		v.mu.Lock()
		n, rerr := data.ReadAt(buf[:want], off)
		v.mu.Unlock()
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
				_ = f.Close()
				_ = os.Remove(tmp)
				return 0, err
			}
			off += int64(n)
		}
		if rerr != nil && rerr != io.EOF {
			_ = f.Close()
			_ = os.Remove(tmp)
			return 0, rerr
		}
		if n == 0 {
			break
		}
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	if off != e.Size {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("read %d of %d bytes", off, e.Size)
	}
	if tmp == target {
		return off, nil // a stream, written in place (its file has the time)
	}
	_ = os.Remove(target)
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	if !e.ModTime.IsZero() {
		_ = os.Chtimes(target, e.ModTime, e.ModTime)
	}
	return off, nil
}

// TempTarget returns the file a restore writes before renaming it to
// target. Alternate data streams on Windows ("file:stream") are written in
// place: a stream cannot be renamed to another stream name.
func TempTarget(target string) string {
	if runtime.GOOS == "windows" && strings.Contains(filepath.Base(target), ":") {
		return target
	}
	return target + ".bzpart"
}

// OpenSnapshotVolume opens the NTFS volume of a partition of sn; ref is a
// PartRef (the partition number for the first disk image).
func OpenSnapshotVolume(ctx context.Context, r *repo.Repository, sn *repo.Snapshot, ref int) (*repo.PartitionImage, *Volume, error) {
	img, part := SplitPartRef(ref)
	if len(sn.Images) == 0 {
		return nil, nil, fmt.Errorf("snapshot %s is not an image backup", sn.ID.Short())
	}
	if img < 0 || img >= len(sn.Images) {
		return nil, nil, fmt.Errorf("image has no disk %d", img)
	}
	for i := range sn.Images[img].Partitions {
		p := &sn.Images[img].Partitions[i]
		if p.Number == part {
			v, err := OpenVolume(ctx, r, p)
			return p, v, err
		}
	}
	return nil, nil, fmt.Errorf("image has no partition %d", part)
}

// DestFunc maps paths inside a partition to restore targets: below target,
// or to the partition's original drive when target is empty. Below a target
// the drive letter becomes the first folder (D:\Restore\C\Users\...), as
// with file backups; a partition without a drive letter keeps its paths.
func DestFunc(p *repo.PartitionImage, target string) (func(string) string, error) {
	if target == "" {
		if len(p.MountPoints) == 0 {
			return nil, fmt.Errorf("partition %d had no drive letter; choose a folder to restore into", p.Number)
		}
		root := p.MountPoints[0]
		return func(path string) string {
			return filepath.Join(root, filepath.FromSlash(strings.ReplaceAll(path, `\`, "/")))
		}, nil
	}
	base := target
	if len(p.MountPoints) > 0 {
		if m := p.MountPoints[0]; len(m) >= 2 && m[1] == ':' && unicode.IsLetter(rune(m[0])) {
			base = filepath.Join(target, strings.ToUpper(m[:1]))
		}
	}
	return func(path string) string {
		return filepath.Join(base, filepath.FromSlash(strings.ReplaceAll(path, `\`, "/")))
	}, nil
}

// ReadFile returns a reader for the contents of a file.
func (v *Volume) ReadFile(p string) (io.Reader, int64, error) {
	e, err := v.Stat(p)
	if err != nil {
		return nil, 0, err
	}
	if e.IsDir {
		return nil, 0, fmt.Errorf("%s is a folder", p)
	}
	v.mu.Lock()
	data, err := v.openData(e.Path)
	v.mu.Unlock()
	if err != nil {
		return nil, 0, err
	}
	return io.NewSectionReader(lockedReaderAt{v, data}, 0, e.Size), e.Size, nil
}

type lockedReaderAt struct {
	v *Volume
	r io.ReaderAt
}

func (l lockedReaderAt) ReadAt(b []byte, off int64) (int, error) {
	l.v.mu.Lock()
	defer l.v.mu.Unlock()
	return l.r.ReadAt(b, off)
}

// SafeName tells whether a name read from a guest file system can be used
// as one path element: not empty, not "." or "..", no slash, backslash or
// NUL. A crafted directory entry could otherwise point a restore outside
// its folder (on Windows a backslash separates folders) or loop forever.
func SafeName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\\x00")
}

// MaxDepth limits how deep restores, downloads and listings walk into a
// file system read from a backup: a crafted one may nest folders without end.
const MaxDepth = 256
