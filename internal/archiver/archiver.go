// Package archiver creates snapshots of files and folders.
package archiver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"time"

	"github.com/backupzit/backupzit/internal/fsutil"
	"github.com/backupzit/backupzit/internal/repo"
	"github.com/restic/chunker"
)

// Options configure a backup run.
type Options struct {
	Paths []string
	Tags  []string
	// Parent is used to skip reading files whose size and mtime are
	// unchanged. If nil, the latest snapshot with the same host and paths
	// is used (unless NoParent is set).
	Parent   *repo.Snapshot
	NoParent bool
	Hostname string
	Version  string
	// Excludes are glob patterns matched against the base name.
	Excludes []string
	// Progress, if set, is called for each file processed.
	Progress func(path string, s *repo.SnapshotStats)
}

// Archiver performs one backup run.
type Archiver struct {
	r     *repo.Repository
	opts  Options
	stats repo.SnapshotStats
	buf   []byte
	chk   *chunker.Chunker
}

// Run backs up opts.Paths and saves a snapshot.
func Run(ctx context.Context, r *repo.Repository, opts Options) (*repo.Snapshot, error) {
	if len(opts.Paths) == 0 {
		return nil, errors.New("no paths to back up")
	}
	if opts.Hostname == "" {
		opts.Hostname, _ = os.Hostname()
	}
	abs := make([]string, 0, len(opts.Paths))
	for _, p := range opts.Paths {
		a, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		if _, err := os.Lstat(a); err != nil {
			return nil, fmt.Errorf("source %s: %w", p, err)
		}
		abs = append(abs, a)
	}
	sort.Strings(abs)
	opts.Paths = abs

	if opts.Parent == nil && !opts.NoParent {
		p, err := findParent(ctx, r, opts.Hostname, abs)
		if err != nil {
			return nil, err
		}
		opts.Parent = p
	}

	a := &Archiver{
		r:    r,
		opts: opts,
		buf:  make([]byte, chunker.MaxSize),
		chk:  chunker.New(nil, r.ChunkerPolynomial()),
	}
	start := time.Now()
	before := r.Stats()

	root := buildVTree(abs)
	var parentTree *repo.Tree
	if opts.Parent != nil {
		t, err := r.LoadTree(ctx, opts.Parent.Tree)
		if err != nil {
			return nil, fmt.Errorf("load parent tree: %w", err)
		}
		parentTree = t
	}
	treeID, err := a.saveVDir(ctx, root, parentTree)
	if err != nil {
		return nil, err
	}
	if err := r.Flush(ctx); err != nil {
		return nil, err
	}

	after := r.Stats()
	a.stats.BytesAdded = after.RawBytes - before.RawBytes
	a.stats.BytesStored = after.StoredBytes - before.StoredBytes
	a.stats.Duration = time.Since(start)

	sn := &repo.Snapshot{
		Time:           start.UTC(),
		Hostname:       opts.Hostname,
		Paths:          abs,
		Tags:           opts.Tags,
		Tree:           treeID,
		Stats:          a.stats,
		ProgramVersion: opts.Version,
	}
	if u, err := user.Current(); err == nil {
		sn.Username = u.Username
	}
	if opts.Parent != nil {
		pid := opts.Parent.ID
		sn.Parent = &pid
	}
	if _, err := r.SaveSnapshot(ctx, sn); err != nil {
		return nil, fmt.Errorf("save snapshot: %w", err)
	}
	return sn, nil
}

func findParent(ctx context.Context, r *repo.Repository, host string, paths []string) (*repo.Snapshot, error) {
	sns, err := r.ListSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	for i := len(sns) - 1; i >= 0; i-- {
		sn := sns[i]
		if sn.Hostname == host && equalStrings(sn.Paths, paths) {
			return sn, nil
		}
	}
	return nil, nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// vdir is an intermediate directory on the way from the root to a source.
type vdir struct {
	realPath string // OS path of this directory ("" for the virtual root)
	source   bool   // this entry is a backup source; archive it entirely
	children map[string]*vdir
}

func buildVTree(paths []string) *vdir {
	root := &vdir{children: map[string]*vdir{}}
	for _, p := range paths {
		comps, err := fsutil.SnapshotComponents(p)
		if err != nil || len(comps) == 0 {
			continue
		}
		cur := root
		for i, c := range comps {
			if cur.source {
				break // an ancestor is already a full source
			}
			child, ok := cur.children[c]
			if !ok {
				child = &vdir{realPath: fsutil.OriginalPath(comps[:i+1]), children: map[string]*vdir{}}
				cur.children[c] = child
			}
			cur = child
		}
		cur.source = true
		cur.children = nil
	}
	return root
}

func (a *Archiver) saveVDir(ctx context.Context, d *vdir, parent *repo.Tree) (repo.ID, error) {
	names := make([]string, 0, len(d.children))
	for n := range d.children {
		names = append(names, n)
	}
	sort.Strings(names)
	t := &repo.Tree{}
	for _, name := range names {
		child := d.children[name]
		var pnode *repo.Node
		if parent != nil {
			pnode = parent.Find(name)
		}
		if child.source {
			n, ok, err := a.archivePath(ctx, child.realPath, name, pnode)
			if err != nil {
				return repo.ID{}, err
			}
			if ok {
				t.Nodes = append(t.Nodes, n)
			}
			continue
		}
		var ptree *repo.Tree
		if pnode != nil && pnode.Type == repo.NodeDir && pnode.Subtree != nil {
			ptree, _ = a.r.LoadTree(ctx, *pnode.Subtree)
		}
		sub, err := a.saveVDir(ctx, child, ptree)
		if err != nil {
			return repo.ID{}, err
		}
		n := repo.Node{Name: name, Type: repo.NodeDir, Mode: uint32(fs.ModeDir | 0o755), Subtree: &sub}
		if fi, err := os.Stat(child.realPath); err == nil {
			n.Mode = uint32(fi.Mode())
			n.ModTime = fi.ModTime().UTC()
			n.WinAttrs = fsutil.WinAttrs(fi)
		}
		t.Nodes = append(t.Nodes, n)
	}
	t.Sort()
	return a.r.SaveTree(ctx, t)
}

func (a *Archiver) addError(path string, err error) {
	a.stats.Errors = append(a.stats.Errors, fmt.Sprintf("%s: %v", path, err))
}

func (a *Archiver) excluded(name string) bool {
	for _, pat := range a.opts.Excludes {
		if ok, _ := filepath.Match(pat, name); ok {
			return true
		}
	}
	return false
}

// archivePath stores the file, directory or symlink at p. ok is false if
// the entry was skipped (error recorded or unsupported type).
func (a *Archiver) archivePath(ctx context.Context, p, name string, parent *repo.Node) (repo.Node, bool, error) {
	if err := ctx.Err(); err != nil {
		return repo.Node{}, false, err
	}
	fi, err := os.Lstat(p)
	if err != nil {
		a.addError(p, err)
		return repo.Node{}, false, nil
	}
	n := repo.Node{
		Name:     name,
		Mode:     uint32(fi.Mode()),
		ModTime:  fi.ModTime().UTC(),
		WinAttrs: fsutil.WinAttrs(fi),
	}
	switch {
	case fi.Mode().IsRegular():
		n.Type = repo.NodeFile
		if err := a.archiveFile(ctx, p, fi, &n, parent); err != nil {
			if ctx.Err() != nil {
				return n, false, ctx.Err()
			}
			var repoErr *repoError
			if errors.As(err, &repoErr) {
				return n, false, repoErr.err
			}
			a.addError(p, err)
			return n, false, nil
		}
		return n, true, nil

	case fi.IsDir():
		n.Type = repo.NodeDir
		a.stats.Dirs++
		var ptree *repo.Tree
		if parent != nil && parent.Type == repo.NodeDir && parent.Subtree != nil {
			ptree, _ = a.r.LoadTree(ctx, *parent.Subtree)
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			a.addError(p, err)
			// Keep the directory itself so the structure is restorable.
		}
		t := &repo.Tree{}
		for _, e := range entries {
			if a.excluded(e.Name()) {
				continue
			}
			var pn *repo.Node
			if ptree != nil {
				pn = ptree.Find(e.Name())
			}
			child, ok, err := a.archivePath(ctx, filepath.Join(p, e.Name()), e.Name(), pn)
			if err != nil {
				return n, false, err
			}
			if ok {
				t.Nodes = append(t.Nodes, child)
			}
		}
		t.Sort()
		id, err := a.r.SaveTree(ctx, t)
		if err != nil {
			return n, false, err
		}
		n.Subtree = &id
		return n, true, nil

	case fi.Mode()&fs.ModeSymlink != 0 || fi.Mode()&fs.ModeIrregular != 0:
		target, err := os.Readlink(p)
		if err != nil {
			a.addError(p, fmt.Errorf("unsupported special file: %w", err))
			return n, false, nil
		}
		n.Type = repo.NodeSymlink
		n.LinkTarget = target
		return n, true, nil

	default:
		// Sockets, devices, named pipes: not backed up.
		return n, false, nil
	}
}

// repoError marks failures of the repository (not of the source file);
// these abort the backup instead of being recorded per file.
type repoError struct{ err error }

func (e *repoError) Error() string { return e.err.Error() }

func (a *Archiver) archiveFile(ctx context.Context, p string, fi fs.FileInfo, n *repo.Node, parent *repo.Node) error {
	size := uint64(fi.Size())
	n.Size = size
	a.stats.Files++
	a.stats.Bytes += size
	if a.opts.Progress != nil {
		a.opts.Progress(p, &a.stats)
	}

	if parent != nil && parent.Type == repo.NodeFile && parent.Size == size &&
		parent.ModTime.Equal(n.ModTime) && a.allBlobsKnown(parent.Content) {
		n.Content = parent.Content
		a.stats.FilesSkipped++
		return nil
	}
	if parent != nil {
		a.stats.FilesChanged++
	} else {
		a.stats.FilesNew++
	}

	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	a.chk.Reset(f, a.r.ChunkerPolynomial())
	var read uint64
	for {
		c, err := a.chk.Next(a.buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		read += uint64(c.Length)
		id, _, err := a.r.SaveBlob(ctx, repo.DataBlob, c.Data)
		if err != nil {
			return &repoError{err}
		}
		n.Content = append(n.Content, id)
	}
	a.stats.BytesRead += read
	if read != size {
		// File changed while reading; the stored content is what we read.
		n.Size = read
		a.stats.Bytes = a.stats.Bytes - size + read
	}
	return nil
}

func (a *Archiver) allBlobsKnown(ids []repo.ID) bool {
	idx := a.r.Index()
	for _, id := range ids {
		if !idx.Has(repo.BlobHandle{Type: repo.DataBlob, ID: id}) {
			return false
		}
	}
	return true
}
