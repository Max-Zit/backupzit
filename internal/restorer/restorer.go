// Package restorer writes snapshot contents back to the filesystem.
package restorer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/backupzit/backupzit/internal/fsutil"
	"github.com/backupzit/backupzit/internal/repo"
	"github.com/restic/chunker"
)

// Options configure a restore.
type Options struct {
	// Target is the directory to restore into. The snapshot's full path
	// layout is recreated below it (Target/C/Users/...). If empty, files are
	// restored to their original locations.
	Target string
	// Include limits the restore to these snapshot paths and everything
	// below them. Accepts OS paths ("C:\Users\a") or snapshot paths ("C/Users/a").
	Include []string
	// Verify re-reads every restored file and compares it with the snapshot.
	Verify bool
	// Progress, if set, is called for each restored file.
	Progress func(path string, s *Stats)
}

// Stats summarises a restore.
type Stats struct {
	Files         uint64
	Dirs          uint64
	Symlinks      uint64
	Bytes         uint64
	FilesVerified uint64
	Duration      time.Duration
	Errors        []string
}

type restorer struct {
	r       *repo.Repository
	opts    Options
	include [][]string
	stats   Stats
	buf     []byte
	chk     *chunker.Chunker
}

// Run restores sn according to opts.
func Run(ctx context.Context, r *repo.Repository, sn *repo.Snapshot, opts Options) (*Stats, error) {
	rs := &restorer{r: r, opts: opts}
	for _, inc := range opts.Include {
		comps, err := fsutil.ParseSnapshotPath(inc)
		if err != nil {
			return nil, err
		}
		rs.include = append(rs.include, comps)
	}
	if opts.Target != "" {
		abs, err := filepath.Abs(opts.Target)
		if err != nil {
			return nil, err
		}
		rs.opts.Target = abs
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return nil, err
		}
	}
	if opts.Verify {
		rs.buf = make([]byte, chunker.MaxSize)
		rs.chk = chunker.New(nil, r.ChunkerPolynomial())
	}
	start := time.Now()
	root, err := r.LoadTree(ctx, sn.Tree)
	if err != nil {
		return nil, fmt.Errorf("load root tree: %w", err)
	}
	if err := rs.restoreTree(ctx, root, nil); err != nil {
		return &rs.stats, err
	}
	if len(rs.include) > 0 && rs.stats.Files+rs.stats.Dirs+rs.stats.Symlinks == 0 {
		return &rs.stats, errors.New("no snapshot entries matched the include paths")
	}
	rs.stats.Duration = time.Since(start)
	return &rs.stats, nil
}

// match reports whether comps should be restored (selected) and whether
// anything below it could be (descend).
func (rs *restorer) match(comps []string) (selected, descend bool) {
	if len(rs.include) == 0 {
		return true, true
	}
	for _, inc := range rs.include {
		if hasPrefix(comps, inc) {
			return true, true // at or below an include
		}
		if hasPrefix(inc, comps) {
			descend = true // ancestor of an include
		}
	}
	return false, descend
}

func hasPrefix(p, prefix []string) bool {
	if len(prefix) > len(p) {
		return false
	}
	for i := range prefix {
		a, b := p[i], prefix[i]
		if runtime.GOOS == "windows" {
			if !strings.EqualFold(a, b) {
				return false
			}
		} else if a != b {
			return false
		}
	}
	return true
}

func (rs *restorer) dest(comps []string) string {
	if rs.opts.Target == "" {
		return fsutil.OriginalPath(comps)
	}
	return filepath.Join(append([]string{rs.opts.Target}, comps...)...)
}

func (rs *restorer) addError(p string, err error) {
	rs.stats.Errors = append(rs.stats.Errors, fmt.Sprintf("%s: %v", p, err))
}

func (rs *restorer) restoreTree(ctx context.Context, t *repo.Tree, prefix []string) error {
	for i := range t.Nodes {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := &t.Nodes[i]
		if !validName(n.Name) {
			rs.addError(strings.Join(prefix, "/")+"/"+n.Name, errors.New("invalid name in snapshot, skipped"))
			continue
		}
		comps := append(append([]string(nil), prefix...), n.Name)
		selected, descend := rs.match(comps)
		if !selected && !descend {
			continue
		}
		dst := rs.dest(comps)
		switch n.Type {
		case repo.NodeDir:
			// Volume roots ("C") map to "C:\" which already exists.
			if err := os.MkdirAll(dst, 0o755); err != nil {
				rs.addError(dst, err)
				continue
			}
			if n.Subtree != nil {
				sub, err := rs.r.LoadTree(ctx, *n.Subtree)
				if err != nil {
					return fmt.Errorf("load tree for %s: %w", dst, err)
				}
				if err := rs.restoreTree(ctx, sub, comps); err != nil {
					return err
				}
			}
			if selected {
				rs.stats.Dirs++
				rs.applyMeta(dst, n, len(comps) > 1)
			}
		case repo.NodeFile:
			if !selected {
				continue
			}
			if err := rs.restoreFile(ctx, dst, n); err != nil {
				rs.addError(dst, err)
				continue
			}
			rs.applyMeta(dst, n, true)
			if rs.opts.Verify {
				if err := rs.verifyFile(dst, n); err != nil {
					rs.addError(dst, fmt.Errorf("verify: %w", err))
				} else {
					rs.stats.FilesVerified++
				}
			}
		case repo.NodeSymlink:
			if !selected {
				continue
			}
			_ = os.Remove(dst)
			if err := os.Symlink(n.LinkTarget, dst); err != nil {
				rs.addError(dst, err)
				continue
			}
			rs.stats.Symlinks++
		default:
			rs.addError(dst, fmt.Errorf("unknown node type %q", n.Type))
		}
	}
	return nil
}

func validName(n string) bool {
	return n != "" && n != "." && n != ".." && !strings.ContainsAny(n, `/\`) && !strings.ContainsRune(n, 0)
}

func (rs *restorer) restoreFile(ctx context.Context, dst string, n *repo.Node) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	fsutil.ClearReadOnly(dst)
	tmp := dst + ".bzpart"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	var written uint64
	for _, id := range n.Content {
		data, err := rs.r.LoadBlob(ctx, repo.DataBlob, id)
		if err != nil {
			f.Close()
			os.Remove(tmp)
			// Missing/corrupt repository data: report but keep restoring others.
			return fmt.Errorf("blob %s: %w", id.Short(), err)
		}
		if _, err := f.Write(data); err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
		written += uint64(len(data))
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if written != n.Size {
		os.Remove(tmp)
		return fmt.Errorf("restored %d bytes, snapshot says %d", written, n.Size)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	rs.stats.Files++
	rs.stats.Bytes += written
	if rs.opts.Progress != nil {
		rs.opts.Progress(dst, &rs.stats)
	}
	return nil
}

func (rs *restorer) applyMeta(dst string, n *repo.Node, setTimes bool) {
	if runtime.GOOS != "windows" {
		if err := os.Chmod(dst, fs.FileMode(n.Mode).Perm()); err != nil {
			rs.addError(dst, err)
		}
	}
	if setTimes && !n.ModTime.IsZero() {
		if err := os.Chtimes(dst, n.ModTime, n.ModTime); err != nil {
			rs.addError(dst, err)
		}
	}
	if runtime.GOOS == "windows" && n.WinAttrs != 0 {
		if err := fsutil.SetWinAttrs(dst, n.WinAttrs); err != nil {
			rs.addError(dst, err)
		}
	}
}

// verifyFile re-chunks the restored file and compares chunk IDs and size.
func (rs *restorer) verifyFile(p string, n *repo.Node) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	rs.chk.Reset(f, rs.r.ChunkerPolynomial())
	var i int
	var size uint64
	for {
		c, err := rs.chk.Next(rs.buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if i >= len(n.Content) || repo.Hash(c.Data) != n.Content[i] {
			return fmt.Errorf("content differs at chunk %d", i)
		}
		size += uint64(c.Length)
		i++
	}
	if i != len(n.Content) || size != n.Size {
		return fmt.Errorf("size/chunk count differs (%d/%d bytes, %d/%d chunks)", size, n.Size, i, len(n.Content))
	}
	return nil
}
