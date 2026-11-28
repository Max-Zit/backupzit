package vmfs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/max-zit/backupzit/internal/fsutil"
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
			// A folder with permissions of its own is created private and
			// gets them once its contents are written (MED 17).
			mkdir := tg.Mkdir
			if e.Perm != 0 {
				mkdir = tg.MkdirPrivate
			}
			if err := mkdir(dst); err != nil {
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
	n, err := copySparse(f, rd)
	if err == nil && tmp != dst {
		// Permissions and times through the file's own handle, before it
		// gets its name: by name, a hard link put in its place could
		// redirect them (0.58).
		_ = f.Chmod(perm)
		if !e.ModTime.IsZero() {
			_ = fsutil.SetTimes(f, e.ModTime)
		}
	}
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
	if err := tg.Replace(tmp, dst); err != nil {
		_ = tg.Remove(tmp)
		return n, err
	}
	return n, nil
}

// sparseBlock is the unit copySparse looks at: a block of zeros is skipped
// with a seek instead of written.
const sparseBlock = 1 << 20

// sparseMarkRun is the shortest run of zeros for which a file is marked
// sparse (Windows; NTFS keeps holes only in a file marked sparse, other
// systems in any file). Hyper-V refuses a sparse virtual disk (B9), so a
// file with only short runs of zeros is written on Windows as before.
const sparseMarkRun = 64 << 20

var zeroBlock [sparseBlock]byte

// copySparse copies rd into the new file f and returns its size. Blocks of
// zeros are skipped instead of written, so a sparse file of the guest stays
// sparse: /var/log/lastlog has an apparent size of (highest UID + 1) × 292
// bytes, about 1.2 TB with UID 4294967294, and was written as zeros onto
// the agent's disk (MED 6a).
func copySparse(f *os.File, rd io.Reader) (int64, error) {
	buf := make([]byte, sparseBlock)
	var off, zeros int64 // zeros: the run of zeros not written yet, ending at off
	var holes []fsutil.Hole
	marked := false
	skip := func() error {
		if zeros == 0 {
			return nil
		}
		if !marked && zeros >= sparseMarkRun {
			// If the file system cannot, the skipped range is filled
			// with zeros, as before.
			_ = fsutil.MarkSparse(f)
			marked = true
		}
		holes = append(holes, fsutil.Hole{Off: off - zeros, N: zeros})
		_, err := f.Seek(zeros, io.SeekCurrent)
		zeros = 0
		return err
	}
	for {
		n, rerr := io.ReadFull(rd, buf)
		if n > 0 {
			if bytes.Equal(buf[:n], zeroBlock[:n]) {
				zeros += int64(n)
			} else {
				if err := skip(); err != nil {
					return off, err
				}
				if _, err := f.Write(buf[:n]); err != nil {
					return off, err
				}
			}
			off += int64(n)
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return off, rerr
		}
	}
	// Zeros at the end: the size alone (the truncate) leaves them as a hole.
	if err := skip(); err != nil {
		return off, err
	}
	if err := f.Truncate(off); err != nil {
		return off, err
	}
	// Where the file system filled skipped ranges with zeros (APFS for
	// gaps below 16 MiB), they are freed again.
	_ = fsutil.PunchHoles(f, holes)
	return off, nil
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
