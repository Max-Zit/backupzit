package vmfs

import (
	"context"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/max-zit/backupzit/internal/imaging"
	"github.com/max-zit/backupzit/internal/repo"
)

// GuestDisk opens a disk of a guest in a Proxmox backup (key "" = first
// disk).
func GuestDisk(ctx context.Context, r *repo.Repository, sn *repo.Snapshot, vmid int, key string) (io.ReaderAt, int64, *repo.GuestDisk, error) {
	for gi := range sn.Guests {
		g := &sn.Guests[gi]
		if g.VMID != vmid {
			continue
		}
		for di := range g.Disks {
			d := &g.Disks[di]
			if key != "" && d.Key != key {
				continue
			}
			if key == "" && !DataDisk(d.Key) {
				continue // UEFI variables, TPM state
			}
			if d.Image < 0 || d.Image >= len(sn.Images) || len(sn.Images[d.Image].Partitions) != 1 {
				return nil, 0, nil, fmt.Errorf("disk %s has no image", d.Key)
			}
			pr, err := imaging.NewPartitionReader(ctx, r, &sn.Images[d.Image].Partitions[0])
			if err != nil {
				return nil, 0, nil, err
			}
			return pr, pr.Size(), d, nil
		}
		if key != "" {
			return nil, 0, nil, fmt.Errorf("guest %d has no disk %s", vmid, key)
		}
		return nil, 0, nil, fmt.Errorf("guest %d has no disks", vmid)
	}
	return nil, 0, nil, fmt.Errorf("snapshot %s does not contain guest %d", sn.ID.Short(), vmid)
}

// FindVolume returns the volume with the given ID, or the first browsable
// one for "".
func FindVolume(vols []Volume, id string) (Volume, error) {
	for _, v := range vols {
		if (id == "" && v.Browsable()) || v.ID == id {
			return v, nil
		}
	}
	if id == "" {
		return Volume{}, ErrNotBrowsable
	}
	return Volume{}, fmt.Errorf("no volume %s", id)
}

// ExtractStats summarises an extraction.
type ExtractStats struct {
	Files  uint64   `json:"files"`
	Dirs   uint64   `json:"dirs"`
	Bytes  uint64   `json:"bytes"`
	Errors []string `json:"errors,omitempty"`
}

// Extract copies paths (files or folders) from fsys below target, keeping
// their path inside the volume (target/etc/nginx/...).
func Extract(ctx context.Context, fsys FS, paths []string, target string) (*ExtractStats, error) {
	st := &ExtractStats{}
	// Every write goes through the restore folder (imaging.Target): a link
	// in it cannot send the restored files elsewhere with the agent's
	// rights, not even one swapped in while the restore runs.
	tg, err := imaging.OpenTarget(target)
	if err != nil {
		return st, err
	}
	defer tg.Close() //nolint:errcheck // the folder handle
	var seen Visited
	var walk func(e Entry, depth int) error
	walk = func(e Entry, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth > MaxDepth {
			st.Errors = append(st.Errors, fmt.Sprintf("%s: more than %d folders deep (damaged file system?), skipped", e.Path, MaxDepth))
			return nil
		}
		dst := filepath.Join(target, filepath.FromSlash(strings.TrimPrefix(e.Path, "/")))
		if !Within(target, dst) {
			st.Errors = append(st.Errors, fmt.Sprintf("%s: outside the restore folder, skipped", e.Path))
			return nil
		}
		// An ext4/XFS name with ':' would become a stream of another file on
		// Windows; NTFS streams (e.Stream) are written as streams.
		if runtime.GOOS == "windows" && !imaging.WindowsName(e.Path, e.Stream) {
			st.Errors = append(st.Errors, imaging.ColonMessage(e.Path))
			return nil
		}
		if e.IsDir {
			if !seen.Enter(e.ID) {
				st.Errors = append(st.Errors, LoopMessage(e.Path))
				return nil
			}
			if err := tg.Mkdir(dst); err != nil {
				st.Errors = append(st.Errors, fmt.Sprintf("%s: %v", e.Path, err))
				return nil
			}
			// The folder's own permissions (e.g. /root or ~/.ssh stay
			// private) once its contents are written: a read-only folder
			// would refuse them.
			if e.Perm != 0 {
				defer tg.Chmod(dst, e.Perm) //nolint:errcheck // as before: the files are written
			}
			st.Dirs++
			children, err := fsys.List(e.Path)
			if err != nil {
				st.Errors = append(st.Errors, fmt.Sprintf("%s: %v", e.Path, err))
				return nil
			}
			for _, c := range children {
				if err := walk(c, depth+1); err != nil {
					return err
				}
			}
			return nil
		}
		n, err := extractFile(fsys, e, tg, dst)
		if err != nil {
			st.Errors = append(st.Errors, fmt.Sprintf("%s: %v", e.Path, err))
			return nil
		}
		st.Files++
		st.Bytes += uint64(n) // #nosec G115 -- a value from a damaged disk that does not fit makes the following read fail
		return nil
	}
	for _, p := range paths {
		e, err := fsys.Stat(p)
		if err != nil {
			st.Errors = append(st.Errors, fmt.Sprintf("%s: %v", p, err))
			continue
		}
		if e.Path == "" {
			e.Path = Clean(p)
		}
		// Per selected path: one selected inside another is copied again,
		// as before, instead of being reported as a loop.
		seen = Visited{}
		if err := walk(e, 0); err != nil {
			return st, err
		}
	}
	return st, nil
}

func extractFile(fsys FS, e Entry, tg *imaging.Target, dst string) (int64, error) {
	rd, _, err := fsys.ReadFile(e.Path)
	if err != nil {
		return 0, err
	}
	if err := tg.Mkdir(filepath.Dir(dst)); err != nil {
		return 0, err
	}
	tmp := imaging.TempTarget(dst)
	// Keep the file's permissions from the guest, so restored secrets
	// (/etc/shadow, private keys) do not become readable for everyone;
	// without them, only the owner may read the file.
	perm := e.Perm
	if perm == 0 {
		perm = 0o600
	}
	f, err := tg.Create(tmp)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, rd)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = tg.Remove(tmp)
		return n, err
	}
	if tmp == dst {
		return n, nil // an NTFS stream on Windows, written in place
	}
	_ = tg.Chmod(tmp, perm)
	if err := tg.Replace(tmp, dst); err != nil {
		_ = tg.Remove(tmp)
		return n, err
	}
	if !e.ModTime.IsZero() {
		_ = tg.Chtimes(dst, e.ModTime, e.ModTime)
	}
	return n, nil
}

// Base returns the last element of a path inside a volume.
func Base(p string) string { return path.Base(Clean(p)) }

// DataDisk reports whether a guest disk holds data (not the UEFI variables
// or TPM state of a VM).
func DataDisk(key string) bool {
	return !strings.HasPrefix(key, "efidisk") && !strings.HasPrefix(key, "tpmstate")
}

// GuestDisks lists the data disks of a guest in a snapshot.
func GuestDisks(sn *repo.Snapshot, vmid int) []string {
	var out []string
	for _, g := range sn.Guests {
		if g.VMID == vmid {
			for _, d := range g.Disks {
				if DataDisk(d.Key) {
					out = append(out, d.Key)
				}
			}
		}
	}
	return out
}
