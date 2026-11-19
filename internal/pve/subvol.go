package pve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/max-zit/backupzit/internal/archiver"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/restorer"
)

// Container volumes on ZFS storage are datasets (subvol-<vmid>-disk-N), not
// block devices. They are read as files from the ZFS snapshot that pct
// snapshot takes, and stored as directories of the repository snapshot's
// tree (<type>/<vmid>/<key>), so prune, check and copy keep them like the
// files of a file backup.

// maxVolumeErrors bounds the unreadable files listed per volume.
const maxVolumeErrors = 20

// isFilesVolume reports whether a volume is stored as files.
func isFilesVolume(st storage, vol string) bool {
	return st.Type == "zfspool" && volumeFormat(vol) == "subvol"
}

// subvolDataset returns the ZFS dataset of a subvolume. Linked clones are
// named basevol-<base>-disk-N/subvol-<vmid>-disk-M; the dataset is the last
// component.
func subvolDataset(st storage, vol string) string {
	return st.Props["pool"] + "/" + filepath.Base(vol)
}

// subvolDir returns the directory to read a subvolume from: the snapshot
// below its mountpoint, or the mountpoint itself for snap "" (templates).
func subvolDir(ctx context.Context, st storage, vol, snap string) (string, error) {
	ds := subvolDataset(st, vol)
	out, err := command(ctx, "zfs", "get", "-H", "-o", "property,value", "mountpoint,mounted", ds)
	if err != nil {
		return "", err
	}
	props := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if k, v, ok := strings.Cut(line, "\t"); ok {
			props[k] = strings.TrimSpace(v)
		}
	}
	mp := props["mountpoint"]
	if !strings.HasPrefix(mp, "/") {
		return "", fmt.Errorf("dataset %s has no mountpoint (%q)", ds, mp)
	}
	if props["mounted"] != "yes" {
		return "", fmt.Errorf("dataset %s is not mounted", ds)
	}
	if snap == "" {
		return mp, nil
	}
	// Reachable also with snapdir=hidden. ZFS mounts the snapshot when it
	// is first read; before that the directory is a stub on another device,
	// and everything below it would look like a mount point to the
	// archiver: mount it now.
	dir := filepath.Join(mp, ".zfs", "snapshot", snap)
	d, err := os.Open(dir) // #nosec G304 -- snapshot of a Proxmox volume
	if err != nil {
		return "", fmt.Errorf("open snapshot %s@%s: %w", ds, snap, err)
	}
	_, err = d.Readdirnames(1)
	_ = d.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read snapshot %s@%s: %w", ds, snap, err)
	}
	return dir, nil
}

// volumePathInTree is where a guest volume stored as files is kept in the
// snapshot's tree.
func volumePathInTree(typ string, vmid int, key string) string {
	return fmt.Sprintf("%s/%d/%s", typ, vmid, key)
}

// archiveVolume stores the files of a subvolume. It returns the volume's
// directory node and the stats of reading it.
func (run guestRun) archiveVolume(ctx context.Context, r *repo.Repository, dir, key, path string) (repo.Node, repo.SnapshotStats, error) {
	var done, read uint64
	n, st, err := archiver.Dir(ctx, r, dir, key, run.prevVolumes[path], archiver.Options{
		OneFileSystem: true,
		Progress: func(_ string, s *repo.SnapshotStats) {
			run.progress(s.Bytes - done)
			done = s.Bytes
			*run.read += s.BytesRead - read
			read = s.BytesRead
		},
	})
	*run.read += st.BytesRead - read
	return n, st, err
}

// volumeErrors renders the unreadable files of a volume as warnings, with
// the snapshot directory replaced by the volume key.
func volumeErrors(key, dir string, errs []string) []string {
	var out []string
	for i, e := range errs {
		if i == maxVolumeErrors {
			out = append(out, fmt.Sprintf("%s: %d more files could not be read", key, len(errs)-i))
			break
		}
		out = append(out, key+": "+strings.Replace(e, dir, "", 1))
	}
	return out
}

// previousVolumes finds, per path in the tree, the newest backup of each
// Proxmox container volume stored as files: unchanged files are taken from
// it instead of being read again. Only backups of this host count: guest IDs
// repeat across hosts (every Proxmox VE starts at 100), and a file of the
// same size and time in another host's container 100 is another file. A
// container moved to another node is read in full once.
func previousVolumes(ctx context.Context, r *repo.Repository, host string) map[string]*repo.Node {
	out := map[string]*repo.Node{}
	snaps, err := r.ListSnapshots(ctx)
	if err != nil {
		return out
	}
	for i := len(snaps) - 1; i >= 0; i-- { // newest first
		sn := snaps[i]
		if sn.Hostname != host {
			continue
		}
		for _, g := range sn.Guests {
			if g.Platform != "proxmox" {
				continue
			}
			for _, d := range g.Disks {
				if !d.Files() || out[d.Path] != nil {
					continue
				}
				if n, err := r.FindNode(ctx, sn.Tree, d.Path); err == nil && n.Type == repo.NodeDir {
					out[d.Path] = n
				}
			}
		}
	}
	return out
}

// saveVolumeTree builds the snapshot's root tree from the volumes stored as
// files of the guests that were backed up.
func saveVolumeTree(ctx context.Context, r *repo.Repository, guests []repo.Guest, vols map[string]repo.Node) (repo.ID, error) {
	type dir struct {
		dirs  map[string]*dir
		nodes []repo.Node
	}
	root := &dir{dirs: map[string]*dir{}}
	for _, g := range guests {
		for _, d := range g.Disks {
			if !d.Files() {
				continue
			}
			n, ok := vols[d.Path]
			if !ok {
				return repo.ID{}, fmt.Errorf("volume %s was not stored", d.Path)
			}
			comps := strings.Split(d.Path, "/")
			cur := root
			for _, c := range comps[:len(comps)-1] {
				next := cur.dirs[c]
				if next == nil {
					next = &dir{dirs: map[string]*dir{}}
					cur.dirs[c] = next
				}
				cur = next
			}
			n.Name = comps[len(comps)-1]
			cur.nodes = append(cur.nodes, n)
		}
	}
	var save func(d *dir) (repo.ID, error)
	save = func(d *dir) (repo.ID, error) {
		t := &repo.Tree{Nodes: append([]repo.Node(nil), d.nodes...)}
		for name, sub := range d.dirs {
			id, err := save(sub)
			if err != nil {
				return repo.ID{}, err
			}
			t.Nodes = append(t.Nodes, repo.Node{Name: name, Type: repo.NodeDir, Mode: uint32(os.ModeDir | 0o755), Subtree: &id})
		}
		t.Sort()
		return r.SaveTree(ctx, t)
	}
	return save(root)
}

// allocSubvol creates a subvolume for a container on a ZFS storage; size 0
// creates it without quota.
func allocSubvol(ctx context.Context, st storage, vmid int, size uint64) (string, error) {
	kib := (size + 1023) / 1024
	out, err := command(ctx, "pvesm", "alloc", st.ID, strconv.Itoa(vmid), "", strconv.FormatUint(kib, 10), "--format", "subvol")
	if err != nil {
		return "", err
	}
	m := createdRe.FindSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("unexpected pvesm output: %s", strings.TrimSpace(string(out)))
	}
	return string(m[1]), nil
}

// writeVolumeFiles restores a volume stored as files into a freshly
// allocated subvolume.
func writeVolumeFiles(ctx context.Context, r *repo.Repository, sn *repo.Snapshot, d repo.GuestDisk, volid string, progress func(uint64)) error {
	n, err := r.FindNode(ctx, sn.Tree, d.Path)
	if err != nil {
		return fmt.Errorf("volume files missing in the snapshot: %w", err)
	}
	dir, err := volumePath(ctx, volid)
	if err != nil {
		return err
	}
	if !mountedDir(dir) {
		return fmt.Errorf("%s is not mounted at %s", volid, dir)
	}
	var done uint64
	st, err := restorer.RunDir(ctx, r, n, dir, restorer.Options{Progress: func(_ string, s *restorer.Stats) {
		progress(s.Bytes - done)
		done = s.Bytes
	}})
	if err != nil {
		return err
	}
	if len(st.Errors) > 0 {
		msg := strings.Join(st.Errors[:min(3, len(st.Errors))], "; ")
		if len(st.Errors) > 3 {
			msg += fmt.Sprintf(" (and %d more)", len(st.Errors)-3)
		}
		return errors.New(msg)
	}
	// Hard links are restored without writing their data again.
	if d.Bytes > done {
		progress(d.Bytes - done)
	}
	return nil
}
