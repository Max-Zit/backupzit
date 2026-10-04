package imaging

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

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
	return &PartitionReader{ctx: ctx, r: r, p: p, ids: ids, size: int64(p.Length),
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
		idx := uint64(off / bs)
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
// files ($MFT, ...) are hidden.
func (v *Volume) List(dir string) ([]Entry, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	dir = CleanPath(dir)
	e, err := v.open(dir)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", dir, err)
	}
	seen := map[string]bool{}
	var out []Entry
	for _, fi := range ntfs.ListDir(v.ctx, e) {
		if fi == nil || fi.IsSlack || fi.Name == "" || fi.Name == "." || fi.NameType == "DOS" {
			continue
		}
		if strings.HasPrefix(fi.Name, "$") && dir == `\` {
			continue
		}
		key := strings.ToLower(fi.Name)
		if seen[key] {
			continue
		}
		seen[key] = true
		p := dir + `\` + fi.Name
		if dir == `\` {
			p = `\` + fi.Name
		}
		out = append(out, Entry{Name: fi.Name, Path: p, IsDir: fi.IsDir, Size: fi.Size, ModTime: fi.Mtime})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
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
	var walk func(e Entry) error
	walk = func(e Entry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		target := dest(e.Path)
		if e.IsDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				st.Errors = append(st.Errors, fmt.Sprintf("%s: %v", e.Path, err))
				return nil
			}
			children, err := v.List(e.Path)
			if err != nil {
				st.Errors = append(st.Errors, fmt.Sprintf("%s: %v", e.Path, err))
				return nil
			}
			for _, c := range children {
				if err := walk(c); err != nil {
					return err
				}
			}
			st.Dirs++
			if !e.ModTime.IsZero() {
				os.Chtimes(target, e.ModTime, e.ModTime)
			}
			return nil
		}
		n, err := v.extractFile(e, target)
		if err != nil {
			st.Errors = append(st.Errors, fmt.Sprintf("%s: %v", e.Path, err))
			return nil
		}
		st.Files++
		st.Bytes += uint64(n)
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
		if err := walk(e); err != nil {
			return st, err
		}
	}
	st.Duration = time.Since(start)
	return st, nil
}

func (v *Volume) extractFile(e Entry, target string) (int64, error) {
	v.mu.Lock()
	mft, err := v.open(e.Path)
	var data ntfs.RangeReaderAt
	if err == nil {
		data, err = ntfs.OpenStream(v.ctx, mft, ntfs.ATTR_TYPE_DATA, ntfs.WILDCARD_STREAM_ID, ntfs.WILDCARD_STREAM_NAME)
	}
	v.mu.Unlock()
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return 0, err
	}
	tmp := target + ".bzpart"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
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
				f.Close()
				os.Remove(tmp)
				return 0, err
			}
			off += int64(n)
		}
		if rerr != nil && rerr != io.EOF {
			f.Close()
			os.Remove(tmp)
			return 0, rerr
		}
		if n == 0 {
			break
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	if off != e.Size {
		os.Remove(tmp)
		return 0, fmt.Errorf("read %d of %d bytes", off, e.Size)
	}
	os.Remove(target)
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	if !e.ModTime.IsZero() {
		os.Chtimes(target, e.ModTime, e.ModTime)
	}
	return off, nil
}

// OpenSnapshotVolume opens the NTFS volume of partition number part of the
// first disk image in sn.
func OpenSnapshotVolume(ctx context.Context, r *repo.Repository, sn *repo.Snapshot, part int) (*repo.PartitionImage, *Volume, error) {
	if len(sn.Images) == 0 {
		return nil, nil, fmt.Errorf("snapshot %s is not an image backup", sn.ID.Short())
	}
	for i := range sn.Images[0].Partitions {
		p := &sn.Images[0].Partitions[i]
		if p.Number == part {
			v, err := OpenVolume(ctx, r, p)
			return p, v, err
		}
	}
	return nil, nil, fmt.Errorf("image has no partition %d", part)
}

// DestFunc maps paths inside a partition to restore targets: below target
// (keeping the folder structure), or to the partition's original drive
// when target is empty.
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
	return func(path string) string {
		return filepath.Join(target, filepath.FromSlash(strings.ReplaceAll(path, `\`, "/")))
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
	mft, err := v.open(e.Path)
	var data ntfs.RangeReaderAt
	if err == nil {
		data, err = ntfs.OpenStream(v.ctx, mft, ntfs.ATTR_TYPE_DATA, ntfs.WILDCARD_STREAM_ID, ntfs.WILDCARD_STREAM_NAME)
	}
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
