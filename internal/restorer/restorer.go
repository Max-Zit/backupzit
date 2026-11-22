// Package restorer writes snapshot contents back to the filesystem.
package restorer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/fsutil"
	"github.com/max-zit/backupzit/internal/repo"
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
	// links maps hard link keys to the first restored file.
	links map[string]firstLink
	// lsrc is the folder of the first file linked to last (lsrcPath).
	lsrc     *fsutil.Dir
	lsrcPath string
	// dir is set by RunDir: the first level holds ordinary folders, not
	// volume roots.
	dir bool
}

// Run restores sn according to opts.
func Run(ctx context.Context, r *repo.Repository, sn *repo.Snapshot, opts Options) (*Stats, error) {
	rs := &restorer{r: r, opts: opts}
	defer rs.closeLinkSource()
	for _, inc := range opts.Include {
		comps, err := fsutil.ParseSnapshotPath(inc)
		if err != nil {
			return nil, err
		}
		rs.include = append(rs.include, comps)
	}
	// The folder the first level of the snapshot goes into: the restore
	// folder, or "/" at the original location (on Windows the first level
	// are the volumes, each opened on its own).
	var top *fsutil.Dir
	switch {
	case opts.Target != "":
		abs, err := filepath.Abs(opts.Target)
		if err != nil {
			return nil, err
		}
		rs.opts.Target = abs
		if top, err = fsutil.OpenFolder(abs); err != nil {
			return nil, err
		}
	case runtime.GOOS != "windows":
		var err error
		if top, err = fsutil.OpenOriginal("/"); err != nil {
			return nil, err
		}
	}
	if top != nil {
		defer top.Close()
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
	if err := rs.restoreTree(ctx, root, nil, top); err != nil {
		return &rs.stats, err
	}
	if len(rs.include) > 0 && rs.stats.Files+rs.stats.Dirs+rs.stats.Symlinks == 0 {
		return &rs.stats, errors.New("no snapshot entries matched the include paths")
	}
	rs.stats.Duration = time.Since(start)
	return &rs.stats, nil
}

// RunDir restores the contents of the directory node dir into target,
// which must exist, and gives target the node's owner, mode and attributes
// (e.g. the root of a container volume). opts.Target and opts.Include are
// not used.
func RunDir(ctx context.Context, r *repo.Repository, dir *repo.Node, target string, opts Options) (*Stats, error) {
	if dir == nil || dir.Type != repo.NodeDir || dir.Subtree == nil {
		return nil, errors.New("not a directory in the snapshot")
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return nil, err
	}
	opts.Target, opts.Include = abs, nil
	rs := &restorer{r: r, opts: opts, dir: true}
	defer rs.closeLinkSource()
	if opts.Verify {
		rs.buf = make([]byte, chunker.MaxSize)
		rs.chk = chunker.New(nil, r.ChunkerPolynomial())
	}
	top, err := fsutil.OpenFolder(abs)
	if err != nil {
		return nil, err
	}
	defer top.Close()
	start := time.Now()
	t, err := r.LoadTree(ctx, *dir.Subtree)
	if err != nil {
		return nil, fmt.Errorf("load root tree: %w", err)
	}
	relock := unlockDir(top) // applySelfMeta sets its stored flags again
	if err := rs.restoreTree(ctx, t, nil, top); err != nil {
		relock()
		return &rs.stats, err
	}
	for _, err := range applySelfMeta(top, dir) {
		rs.addError(abs, err)
	}
	rs.stats.Dirs++
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

// restoreTree restores the entries of t, which belong into the folder dir
// (the folder of prefix). dir is nil only above the volumes on Windows at
// the original location (no folder holds "C" or "UNC").
func (rs *restorer) restoreTree(ctx context.Context, t *repo.Tree, prefix []string, dir *fsutil.Dir) error {
	// A folder never holds two entries of one name. A tampered snapshot
	// could pair a symlink with a folder of the same name, so that the
	// folder's files are written where the link points: skip duplicates.
	seen := make(map[string]bool, len(t.Nodes))
	for i := range t.Nodes {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := &t.Nodes[i]
		if !validName(n.Name) {
			rs.addError(strings.Join(prefix, "/")+"/"+n.Name, errors.New("invalid name in snapshot, skipped"))
			continue
		}
		key := n.Name
		if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
			key = strings.ToLower(key) // case-insensitive file systems
		}
		if seen[key] {
			rs.addError(strings.Join(prefix, "/")+"/"+n.Name, errors.New("duplicate name in snapshot, skipped"))
			continue
		}
		seen[key] = true
		if rs.dir && len(prefix) == 0 && n.Name == ".zfs" {
			// The snapshot folder of a ZFS file system: writing there would
			// create or remove ZFS snapshots, never files.
			rs.addError(n.Name, errors.New("ZFS snapshot folder in the backup, skipped"))
			continue
		}
		comps := append(append([]string(nil), prefix...), n.Name)
		selected, descend := rs.match(comps)
		if !selected && !descend {
			continue
		}
		dst := rs.dest(comps)
		if n.Type != repo.NodeDir && dir == nil {
			rs.addError(dst, errors.New("not inside a folder, skipped"))
			continue
		}
		switch n.Type {
		case repo.NodeDir:
			sub, err := rs.openSub(dir, comps, dst)
			if err != nil {
				rs.addError(dst, err)
				continue
			}
			// A folder locked by an earlier restore takes no new entries
			// (macOS only; relock: its flags back when its metadata is not set).
			relock := unlockDir(sub)
			metaSet := false
			if n.Subtree != nil {
				st, err := rs.r.LoadTree(ctx, *n.Subtree)
				if err == nil {
					err = rs.restoreTree(ctx, st, comps, sub)
				} else {
					err = fmt.Errorf("load tree for %s: %w", dst, err)
				}
				if err != nil {
					if sub != nil {
						relock()
						_ = sub.Close()
					}
					return err
				}
			}
			if selected {
				rs.stats.Dirs++
				// A volume root ("C") keeps the attributes of the folder: C:\ is
				// hidden and system, which would hide everything restored below it.
				if sub != nil && (len(comps) > 1 || rs.dir || runtime.GOOS != "windows") {
					setTimes := len(comps) > 1 || rs.dir
					for _, err := range applyDirMeta(dir, sub, n.Name, n, setTimes) {
						rs.addError(dst, err)
					}
					metaSet = setTimes // flags are set with the times
				}
			}
			if sub != nil {
				if !metaSet {
					relock()
				}
				_ = sub.Close()
			}
		case repo.NodeFile:
			if !selected {
				continue
			}
			if n.Unix != nil && n.Unix.LinkKey != "" {
				if first, ok := rs.links[n.Unix.LinkKey]; ok {
					if err := rs.linkTo(dir, n.Name, first); err == nil {
						rs.stats.Files++
						continue
					}
					// Fall back to a copy (e.g. across file systems).
				}
			}
			id, err := rs.restoreFile(ctx, dir, n.Name, dst, n)
			if err != nil {
				rs.addError(dst, err)
				continue
			}
			if n.Unix != nil && n.Unix.LinkKey != "" {
				if rs.links == nil {
					rs.links = map[string]firstLink{}
				}
				if _, ok := rs.links[n.Unix.LinkKey]; !ok {
					rs.links[n.Unix.LinkKey] = firstLink{dst, id}
				}
			}
			if rs.opts.Verify {
				if err := rs.verifyFile(dir, n.Name, n); err != nil {
					rs.addError(dst, fmt.Errorf("verify: %w", err))
				} else {
					rs.stats.FilesVerified++
				}
			}
		case repo.NodeSymlink:
			if !selected {
				continue
			}
			if err := dir.Symlink(n.LinkTarget, n.Name); err != nil {
				rs.addError(dst, err)
				continue
			}
			rs.stats.Symlinks++
			for _, err := range applyLinkMeta(dir, n.Name, n) {
				rs.addError(dst, err)
			}
		case repo.NodeDev, repo.NodeFifo:
			if !selected {
				continue
			}
			for _, err := range makeSpecialAt(dir, n.Name, n) {
				rs.addError(dst, err)
			}
		default:
			rs.addError(dst, fmt.Errorf("unknown node type %q", n.Type))
		}
	}
	return nil
}

// openSub opens the folder of comps (dst) for restoring into it, creating
// it: in dir, or from the volume root for the first folders on Windows at
// the original location. Below a restore folder a link where a folder
// belongs is replaced by a real folder: one put there by root in a
// container, or by a forged backup, would send the restored files where it
// points. At the original location a link of the system (owned by root or
// the agent's user, e.g. /bin -> usr/bin; on Windows SYSTEM, TrustedInstaller
// or an administrator) is followed; one of another user is not. A nil
// folder with no error: a level above the shares of a UNC path.
func (rs *restorer) openSub(dir *fsutil.Dir, comps []string, dst string) (*fsutil.Dir, error) {
	original := rs.opts.Target == "" && !rs.dir
	if original && runtime.GOOS == "windows" {
		switch {
		case len(comps) == 1 && len(comps[0]) == 1: // a volume: "C" is C:\
			return fsutil.OpenOriginal(dst)
		case comps[0] == "UNC" && len(comps) < 3:
			return nil, nil
		case comps[0] == "UNC" && len(comps) == 3: // \server\share
			return fsutil.OpenOriginal(dst)
		}
	}
	if dir == nil {
		return nil, errors.New("not inside a folder, skipped")
	}
	return dir.Sub(comps[len(comps)-1], original)
}

// openDirOf opens the folder p again (for a hard link to a file restored
// earlier), the same way as while restoring.
func (rs *restorer) openDirOf(p string) (*fsutil.Dir, error) {
	if rs.opts.Target == "" && !rs.dir {
		return fsutil.OpenOriginal(p)
	}
	rel, err := filepath.Rel(rs.opts.Target, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("%s is outside the restore folder", p)
	}
	d, err := fsutil.OpenFolder(rs.opts.Target)
	if err != nil {
		return nil, err
	}
	if rel == "." {
		return d, nil
	}
	for _, c := range strings.Split(rel, string(filepath.Separator)) {
		next, err := d.Sub(c, false)
		_ = d.Close()
		if err != nil {
			return nil, err
		}
		d = next
	}
	return d, nil
}

// hole is a range of zeros skipped instead of written (offset, length).
type hole struct{ off, n int64 }

// linkTo creates name in dir as a hard link to the file restored at first.
// firstLink is the first restored file of a set of hard links.
type firstLink struct {
	path string
	id   fsutil.FileID
}

// linkTo creates name in dir as a hard link to the file restored first;
// the folder of that file stays open for the next link to it.
func (rs *restorer) linkTo(dir *fsutil.Dir, name string, first firstLink) error {
	fd := filepath.Dir(first.path)
	if rs.lsrc == nil || rs.lsrcPath != fd {
		rs.closeLinkSource()
		src, err := rs.openDirOf(fd)
		if err != nil {
			return err
		}
		rs.lsrc, rs.lsrcPath = src, fd
	}
	return dir.LinkFrom(rs.lsrc, filepath.Base(first.path), name, first.id)
}

func (rs *restorer) closeLinkSource() {
	if rs.lsrc != nil {
		_ = rs.lsrc.Close()
		rs.lsrc, rs.lsrcPath = nil, ""
	}
}

func validName(n string) bool {
	if n == "" || n == "." || n == ".." || strings.ContainsRune(n, 0) || strings.ContainsRune(n, '/') {
		return false
	}
	// A backslash is an ordinary character in Unix names (systemd units such
	// as system-systemd\x2dcryptsetup.slice) but a separator on Windows.
	return runtime.GOOS != "windows" || !strings.ContainsRune(n, '\\')
}

// restoreFile writes the file name in dir and returns its identity (for hard
// links to it).
func (rs *restorer) restoreFile(ctx context.Context, dir *fsutil.Dir, name, dst string, n *repo.Node) (fsutil.FileID, error) {
	dir.ClearReadOnly(name)
	tmp := name + ".bzpart"
	// Created exclusively in the opened folder: a file or link left at the
	// temporary name (e.g. put there by root in a container) is removed,
	// never written through.
	var none fsutil.FileID
	f, err := dir.CreateExcl(tmp)
	if err != nil {
		return none, err
	}
	fail := func(err error) (fsutil.FileID, error) {
		_ = f.Close()
		_ = dir.Remove(tmp)
		return none, err
	}
	var written uint64
	// Only a file that was sparse is marked sparse again: Hyper-V refuses
	// a sparse VHD/VHDX ("must not be sparse"), and backups before 0.57
	// did not record the attribute (those files stay as they were
	// restored before).
	marked := n.WinAttrs&fsutil.AttrSparse == 0 // true: nothing to mark
	var holes []hole
	for _, id := range n.Content {
		data, err := rs.r.LoadBlob(ctx, repo.DataBlob, id)
		if err != nil {
			// Missing/corrupt repository data: report but keep restoring others.
			return fail(fmt.Errorf("blob %s: %w", id.Short(), err))
		}
		// A chunk of zeros is skipped instead of written: sparse files
		// (VM images, databases) stay sparse instead of taking their full
		// size; the file system reads the hole as zeros.
		if allZero(data) {
			if !marked {
				// Windows needs the file marked sparse before the first
				// hole; if the file system cannot, the zeros are written.
				// In a file that is not marked NTFS writes the zeros too.
				_ = markSparse(f)
				marked = true
			}
			// #nosec G115 -- file sizes are below 2^63
			if last := len(holes) - 1; last >= 0 && holes[last].off+holes[last].n == int64(written) {
				holes[last].n += int64(len(data)) // one range for consecutive chunks of zeros
			} else {
				holes = append(holes, hole{int64(written), int64(len(data))})
			}
			_, err = f.Seek(int64(len(data)), io.SeekCurrent)
		} else {
			_, err = f.Write(data)
		}
		if err != nil {
			return fail(err)
		}
		written += uint64(len(data))
	}
	// A hole at the end still needs the file's size.
	if err := f.Truncate(int64(written)); err != nil { // #nosec G115 -- file sizes are below 2^63
		return fail(err)
	}
	// Where the file system filled skipped ranges with zeros (APFS does
	// for gaps below 16 MiB), they are freed again; if it cannot, the
	// zeros stay, as before.
	_ = punchHoles(f, holes)
	if written != n.Size {
		return fail(fmt.Errorf("restored %d bytes, snapshot says %d", written, n.Size))
	}
	// Owner, mode, attributes and times through the file's own handle
	// (Linux), before it gets its name.
	metaErrs := applyFileMeta(f, n, true)
	id, _ := fsutil.IDOf(f)
	if err := f.Close(); err != nil {
		_ = dir.Remove(tmp)
		return none, err
	}
	if err := dir.Rename(tmp, name); err != nil {
		_ = dir.Remove(tmp)
		return none, err
	}
	metaErrs = append(metaErrs, afterRename(dir, name, id, n)...)
	for _, err := range metaErrs {
		rs.addError(dst, err)
	}
	rs.stats.Files++
	rs.stats.Bytes += written
	if rs.opts.Progress != nil {
		rs.opts.Progress(dst, &rs.stats)
	}
	return id, nil
}

// verifyFile reads the restored file back in pieces of the stored chunks'
// lengths and compares their hashes and the total size. It does not depend
// on the repository's chunker, so it also works for copied snapshots.
func (rs *restorer) verifyFile(dir *fsutil.Dir, name string, n *repo.Node) error {
	f, err := dir.Root().Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	var size uint64
	for i, id := range n.Content {
		l, ok := rs.r.BlobLength(id)
		if !ok {
			return fmt.Errorf("chunk %d not in index", i)
		}
		if cap(rs.buf) < l {
			rs.buf = make([]byte, l)
		}
		b := rs.buf[:l]
		if _, err := io.ReadFull(f, b); err != nil {
			return fmt.Errorf("content differs at chunk %d: %w", i, err)
		}
		if repo.Hash(b) != id {
			return fmt.Errorf("content differs at chunk %d", i)
		}
		size += uint64(l) // #nosec G115 -- file sizes and counts are not negative
	}
	if extra, _ := f.Read(make([]byte, 1)); extra > 0 || size != n.Size {
		return fmt.Errorf("size differs (%d bytes restored, %d expected)", size, n.Size)
	}
	return nil
}

var zeroBlock [64 << 10]byte

func allZero(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for len(b) > 0 {
		n := min(len(b), len(zeroBlock))
		if !bytes.Equal(b[:n], zeroBlock[:n]) {
			return false
		}
		b = b[n:]
	}
	return true
}
