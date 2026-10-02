package vmfs

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/backupzit/backupzit/internal/imaging"
	"github.com/backupzit/backupzit/internal/repo"
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
	var walk func(e Entry) error
	walk = func(e Entry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		dst := filepath.Join(target, filepath.FromSlash(strings.TrimPrefix(e.Path, "/")))
		if e.IsDir {
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
			st.Dirs++
			children, err := fsys.List(e.Path)
			if err != nil {
				st.Errors = append(st.Errors, fmt.Sprintf("%s: %v", e.Path, err))
				return nil
			}
			for _, c := range children {
				if err := walk(c); err != nil {
					return err
				}
			}
			return nil
		}
		n, err := extractFile(fsys, e, dst)
		if err != nil {
			st.Errors = append(st.Errors, fmt.Sprintf("%s: %v", e.Path, err))
			return nil
		}
		st.Files++
		st.Bytes += uint64(n)
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
		if err := walk(e); err != nil {
			return st, err
		}
	}
	return st, nil
}

func extractFile(fsys FS, e Entry, dst string) (int64, error) {
	rd, _, err := fsys.ReadFile(e.Path)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}
	tmp := dst + ".bzpart"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, rd)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return n, err
	}
	os.Remove(dst)
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return n, err
	}
	if !e.ModTime.IsZero() {
		os.Chtimes(dst, e.ModTime, e.ModTime)
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
