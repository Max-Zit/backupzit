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
	"time"

	"github.com/backupzit/backupzit/internal/diskimg"
	"github.com/backupzit/backupzit/internal/repo"
)

// BackupOptions select the guests to back up.
type BackupOptions struct {
	// VMIDs lists the guests; nil means all guests on this node.
	VMIDs []int
	// Exclude lists guests skipped when VMIDs is nil.
	Exclude  []int
	Hostname string
	Version  string
	Tags     []string
	// Progress is called with bytes read and total bytes to read.
	Progress func(done, total uint64)
	// Log receives progress messages.
	Log func(msg string, args ...any)
}

// Backup snapshots the selected guests and stores their disks and
// configurations in one repository snapshot. Guests that fail are listed in
// the snapshot's errors; Backup fails only if no guest could be saved.
func Backup(ctx context.Context, r *repo.Repository, opts BackupOptions) (*repo.Snapshot, error) {
	if opts.Log == nil {
		opts.Log = func(string, ...any) {}
	}
	inv, err := GetInventory(ctx)
	if err != nil {
		return nil, fmt.Errorf("list guests: %w", err)
	}
	if opts.Hostname == "" {
		opts.Hostname = inv.Node
	}
	stores, err := readStorage()
	if err != nil {
		return nil, fmt.Errorf("read storage configuration: %w", err)
	}

	byID := map[int]Guest{}
	for _, g := range inv.Guests {
		byID[g.VMID] = g
	}
	var guests []Guest
	var stats repo.SnapshotStats
	addErr := func(item string, err error) { stats.Errors = append(stats.Errors, fmt.Sprintf("%s: %v", item, err)) }
	if opts.VMIDs == nil {
		excl := map[int]bool{}
		for _, id := range opts.Exclude {
			excl[id] = true
		}
		for _, g := range inv.Guests {
			if g.Node == inv.Node && !excl[g.VMID] {
				guests = append(guests, g)
			}
		}
	} else {
		for _, id := range opts.VMIDs {
			g, ok := byID[id]
			switch {
			case !ok:
				addErr(fmt.Sprintf("guest %d", id), errors.New("does not exist (any more)"))
			case g.Node != inv.Node:
				addErr(guestLabel(g), fmt.Errorf("runs on node %s; this agent is on %s — install the BackupZit agent on %s", g.Node, inv.Node, g.Node))
			default:
				guests = append(guests, g)
			}
		}
	}
	if len(guests) == 0 && len(stats.Errors) == 0 {
		return nil, errors.New("no virtual machines or containers to back up on this node")
	}

	// Disk sizes for progress.
	var total, done uint64
	for _, g := range guests {
		total += configuredSize(g)
	}
	progress := func(n uint64) {
		done += n
		if opts.Progress != nil {
			opts.Progress(done, max(total, done))
		}
	}

	start := time.Now()
	before := r.Stats()
	sn := &repo.Snapshot{
		Time:           start.UTC(),
		Hostname:       opts.Hostname,
		Tags:           append([]string{"proxmox"}, opts.Tags...),
		ProgramVersion: opts.Version,
	}
	for _, g := range guests {
		opts.Log("backing up guest", "vmid", g.VMID, "name", g.Name)
		gi, warns, err := backupGuest(ctx, r, stores, g, sn, progress)
		for _, w := range warns {
			addErr(guestLabel(g), errors.New(w))
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			addErr(guestLabel(g), err)
			continue
		}
		sn.Guests = append(sn.Guests, *gi)
		sn.Paths = append(sn.Paths, fmt.Sprintf("%s/%d", g.Type, g.VMID))
		stats.Files++
	}
	if len(sn.Guests) == 0 {
		return nil, fmt.Errorf("no guest could be backed up: %s", strings.Join(stats.Errors, "; "))
	}
	for _, img := range sn.Images {
		stats.Bytes += img.Partitions[0].StoredBytes
		stats.BytesRead += img.Size
	}

	treeID, err := r.SaveTree(ctx, &repo.Tree{})
	if err != nil {
		return nil, err
	}
	if err := r.Flush(ctx); err != nil {
		return nil, err
	}
	after := r.Stats()
	stats.BytesAdded = after.RawBytes - before.RawBytes
	stats.BytesStored = after.StoredBytes - before.StoredBytes
	stats.Duration = time.Since(start)
	sn.Tree = treeID
	sn.Stats = stats
	if _, err := r.SaveSnapshot(ctx, sn); err != nil {
		return nil, fmt.Errorf("save snapshot: %w", err)
	}
	return sn, nil
}

func guestLabel(g Guest) string {
	kind := "VM"
	if g.Type == "lxc" {
		kind = "CT"
	}
	if g.Name != "" {
		return fmt.Sprintf("%s %d (%s)", kind, g.VMID, g.Name)
	}
	return fmt.Sprintf("%s %d", kind, g.VMID)
}

// snapshotCapable reports whether a volume can be read from a guest
// snapshot.
func snapshotCapable(st storage, volname string) bool {
	switch st.Type {
	case "lvmthin", "rbd":
		return true
	case "zfspool":
		return volumeFormat(volname) != "subvol"
	case "dir", "nfs", "cifs", "glusterfs", "cephfs":
		return volumeFormat(volname) == "qcow2"
	}
	return false
}

func backupGuest(ctx context.Context, r *repo.Repository, stores map[string]storage, g Guest, sn *repo.Snapshot, progress func(uint64)) (*repo.Guest, []string, error) {
	cfgPath, err := configPath(g.Type, g.VMID)
	if err != nil {
		return nil, nil, err
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, nil, err
	}
	conf := string(raw)
	disks, warns := configDisks(g.Type, conf)

	running := g.Status == "running"
	useSnap := !g.Template
	for _, d := range disks {
		sid, vol, _ := strings.Cut(d.Volume, ":")
		st, ok := stores[sid]
		if !ok {
			return nil, warns, fmt.Errorf("%s: unknown storage %q", d.Key, sid)
		}
		if !snapshotCapable(st, vol) {
			if d.CloudInit {
				continue
			}
			if volumeFormat(vol) == "subvol" {
				return nil, warns, fmt.Errorf("%s: ZFS subvolumes of containers are not supported yet", d.Key)
			}
			useSnap = false
		}
	}
	if !useSnap && running {
		return nil, warns, errors.New("the guest is running and one of its disks is on storage without snapshot support (" +
			"supported: LVM-thin, ZFS, Ceph RBD, qcow2 files); stop it during the backup or move the disk")
	}

	gi := &repo.Guest{
		Platform: "proxmox", Type: g.Type, VMID: g.VMID, Name: g.Name, Node: g.Node, Running: running,
		Config: cleanConfig(conf),
	}
	snap := ""
	if useSnap {
		// Remove snapshots left behind by an interrupted backup.
		for _, n := range snapshotNames(conf) {
			if strings.HasPrefix(n, snapPrefix) {
				command(ctx, guestTool(g.Type), "delsnapshot", strconv.Itoa(g.VMID), n, "--force")
			}
		}
		snap = snapPrefix + strconv.FormatInt(time.Now().Unix(), 10)
		args := []string{"snapshot", strconv.Itoa(g.VMID), snap, "--description", "Temporary snapshot for a BackupZit backup; removed automatically"}
		if g.Type == "qemu" {
			args = append(args, "--vmstate", "0")
		}
		frozen := running && g.Type == "qemu" && agentRunning(ctx, g.VMID)
		if _, err := command(ctx, guestTool(g.Type), args...); err != nil {
			return nil, warns, fmt.Errorf("create snapshot: %w", err)
		}
		defer func() {
			cctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if _, err := command(cctx, guestTool(g.Type), "delsnapshot", strconv.Itoa(g.VMID), snap); err != nil {
				command(cctx, guestTool(g.Type), "delsnapshot", strconv.Itoa(g.VMID), snap, "--force")
			}
		}()
		switch {
		case !running:
			gi.Consistency = "snapshot of the stopped guest"
		case g.Type == "lxc":
			gi.Consistency = "snapshot of the running container"
		case frozen:
			gi.Consistency = "snapshot, file systems frozen by the QEMU guest agent"
		default:
			gi.Consistency = "crash-consistent snapshot (no QEMU guest agent running in the VM)"
		}
	} else {
		gi.Consistency = "stopped guest, disks read directly"
		if g.Template {
			gi.Consistency = "template, disks read directly"
		}
	}

	for _, d := range disks {
		if d.CloudInit {
			// Regenerated from the configuration on restore.
			continue
		}
		sid, vol, _ := strings.Cut(d.Volume, ":")
		st := stores[sid]
		var src *source
		if snap != "" {
			src, err = openSnapshot(ctx, st, vol, d.Volume, snap)
		} else {
			src, err = openLive(ctx, st, vol, d.Volume)
		}
		if err != nil {
			return nil, warns, fmt.Errorf("%s (%s): %w", d.Key, d.Volume, err)
		}
		img, err := imageDisk(ctx, r, src, len(sn.Images), fmt.Sprintf("%s/%d %s %s", g.Type, g.VMID, d.Key, d.Volume), progress)
		src.close()
		if err != nil {
			return nil, warns, fmt.Errorf("%s (%s): %w", d.Key, d.Volume, err)
		}
		sn.Images = append(sn.Images, img)
		gi.Disks = append(gi.Disks, repo.GuestDisk{
			Key: d.Key, Volume: d.Volume, Storage: sid, Format: volumeFormat(vol), Size: img.Size, Image: len(sn.Images) - 1,
		})
	}
	if len(gi.Disks) == 0 {
		warns = append(warns, "has no disks to back up; only the configuration was saved")
	}
	return gi, warns, nil
}

// cleanConfig keeps the main configuration section without the snapshot
// reference and lock.
func cleanConfig(conf string) string {
	var b strings.Builder
	for _, line := range strings.Split(mainSection(conf), "\n") {
		if strings.HasPrefix(line, "parent:") || strings.HasPrefix(line, "lock:") {
			continue
		}
		if line == "" {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func agentRunning(ctx context.Context, vmid int) bool {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := command(cctx, "qm", "agent", strconv.Itoa(vmid), "ping")
	return err == nil
}

// source is a readable disk: a snapshot device or file.
type source struct {
	path    string
	cleanup []func()
}

func (s *source) close() {
	for i := len(s.cleanup) - 1; i >= 0; i-- {
		s.cleanup[i]()
	}
}

func bg() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 2*time.Minute)
}

// openSnapshot makes the snapshot of a volume readable.
func openSnapshot(ctx context.Context, st storage, vol, volid, snap string) (*source, error) {
	switch st.Type {
	case "lvmthin":
		vg := st.Props["vgname"]
		lv := "snap_" + vol + "_" + snap
		if _, err := command(ctx, "lvchange", "-ay", "-Ky", vg+"/"+lv); err != nil {
			return nil, err
		}
		s := &source{path: "/dev/" + vg + "/" + lv}
		s.cleanup = append(s.cleanup, func() { c, cancel := bg(); defer cancel(); command(c, "lvchange", "-an", vg+"/"+lv) })
		if err := waitPath(ctx, s.path); err != nil {
			s.close()
			return nil, err
		}
		return s, nil
	case "zfspool":
		ds := st.Props["pool"] + "/" + vol
		prev, _ := command(ctx, "zfs", "get", "-H", "-o", "value,source", "snapdev", ds)
		if _, err := command(ctx, "zfs", "set", "snapdev=visible", ds); err != nil {
			return nil, err
		}
		s := &source{path: "/dev/zvol/" + ds + "@" + snap}
		s.cleanup = append(s.cleanup, func() {
			c, cancel := bg()
			defer cancel()
			f := strings.Fields(string(prev))
			if len(f) == 2 && f[1] == "local" {
				command(c, "zfs", "set", "snapdev="+f[0], ds)
			} else {
				command(c, "zfs", "inherit", "snapdev", ds)
			}
		})
		if err := waitPath(ctx, s.path); err != nil {
			s.close()
			return nil, err
		}
		return s, nil
	case "rbd":
		args := append([]string{"map", "--read-only"}, rbdArgs(st)...)
		args = append(args, rbdImage(st, vol)+"@"+snap)
		out, err := command(ctx, "rbd", args...)
		if err != nil {
			return nil, err
		}
		dev := strings.TrimSpace(string(out))
		s := &source{path: dev}
		s.cleanup = append(s.cleanup, func() { c, cancel := bg(); defer cancel(); command(c, "rbd", "unmap", dev) })
		return s, nil
	default: // file based: qcow2 internal snapshot
		file, err := volumePath(ctx, volid)
		if err != nil {
			return nil, err
		}
		return nbdConnect(ctx, file, volumeFormat(vol), snap)
	}
}

// openLive opens a volume of a stopped guest.
func openLive(ctx context.Context, st storage, vol, volid string) (*source, error) {
	switch st.Type {
	case "lvmthin", "lvm":
		vg := st.Props["vgname"]
		command(ctx, "lvchange", "-ay", vg+"/"+vol)
	case "rbd":
		args := append([]string{"map", "--read-only"}, rbdArgs(st)...)
		args = append(args, rbdImage(st, vol))
		out, err := command(ctx, "rbd", args...)
		if err != nil {
			return nil, err
		}
		dev := strings.TrimSpace(string(out))
		s := &source{path: dev}
		s.cleanup = append(s.cleanup, func() { c, cancel := bg(); defer cancel(); command(c, "rbd", "unmap", dev) })
		return s, nil
	}
	p, err := volumePath(ctx, volid)
	if err != nil {
		return nil, err
	}
	if f := volumeFormat(vol); f == "qcow2" || f == "vmdk" {
		return nbdConnect(ctx, p, f, "")
	}
	if err := waitPath(ctx, p); err != nil {
		return nil, err
	}
	return &source{path: p}, nil
}

func volumePath(ctx context.Context, volid string) (string, error) {
	out, err := command(ctx, "pvesm", "path", volid)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func rbdImage(st storage, vol string) string {
	pool := st.Props["pool"]
	if pool == "" {
		pool = "rbd"
	}
	if ns := st.Props["namespace"]; ns != "" {
		return pool + "/" + ns + "/" + vol
	}
	return pool + "/" + vol
}

func rbdArgs(st storage) []string {
	if st.Props["monhost"] == "" {
		return nil // hyper-converged Ceph: /etc/ceph/ceph.conf
	}
	user := st.Props["username"]
	if user == "" {
		user = "admin"
	}
	mon := strings.Join(strings.FieldsFunc(st.Props["monhost"], func(r rune) bool { return r == ' ' || r == ';' || r == ',' }), ",")
	return []string{"-m", mon, "--id", user, "--keyring", filepath.Join(ConfigDir, "priv", "ceph", st.ID+".keyring")}
}

// nbdConnect exposes an image file (optionally an internal snapshot of it)
// as a read-only /dev/nbdN device with qemu-nbd.
func nbdConnect(ctx context.Context, file, format, snap string) (*source, error) {
	command(ctx, "modprobe", "nbd", "max_part=0")
	for i := 0; i < 64; i++ {
		dev := fmt.Sprintf("/dev/nbd%d", i)
		sys := fmt.Sprintf("/sys/block/nbd%d", i)
		if _, err := os.Stat(sys); err != nil {
			break
		}
		if _, err := os.Stat(sys + "/pid"); err == nil {
			continue // in use
		}
		args := []string{"--read-only", "--force-share", "--format", format, "--connect", dev}
		if snap != "" {
			args = append(args, "--load-snapshot", snap)
		}
		args = append(args, file)
		if _, err := command(ctx, "qemu-nbd", args...); err != nil {
			if strings.Contains(err.Error(), "busy") || strings.Contains(err.Error(), "in use") {
				continue
			}
			return nil, err
		}
		s := &source{path: dev}
		s.cleanup = append(s.cleanup, func() { c, cancel := bg(); defer cancel(); command(c, "qemu-nbd", "--disconnect", dev) })
		// Wait until the kernel sees the size.
		deadline := time.Now().Add(15 * time.Second)
		for {
			b, _ := os.ReadFile(sys + "/size")
			if n, _ := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64); n > 0 {
				return s, nil
			}
			if time.Now().After(deadline) {
				s.close()
				return nil, fmt.Errorf("%s did not become ready", dev)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	return nil, errors.New("no free /dev/nbd device")
}

// imageDisk stores a disk block by block. All-zero blocks are recorded as
// null IDs and not stored.
func imageDisk(ctx context.Context, r *repo.Repository, src *source, number int, model string, progress func(uint64)) (repo.DiskImage, error) {
	f, err := os.Open(src.path)
	if err != nil {
		return repo.DiskImage{}, err
	}
	defer f.Close()
	end, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return repo.DiskImage{}, fmt.Errorf("size of %s: %w", src.path, err)
	}
	size := uint64(end)
	if size == 0 {
		return repo.DiskImage{}, fmt.Errorf("%s is empty", src.path)
	}
	return diskimg.Store(ctx, r, f, size, number, model, "snapshot", progress)
}

// configuredSize sums the disk sizes in a guest's configuration.
func configuredSize(g Guest) uint64 {
	p, err := configPath(g.Type, g.VMID)
	if err != nil {
		return g.MaxDisk
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return g.MaxDisk
	}
	disks, _ := configDisks(g.Type, string(b))
	var n uint64
	for _, d := range disks {
		if !d.CloudInit {
			s, _ := parseSize(optValue(d.Opts, "size"))
			n += s
		}
	}
	return n
}
