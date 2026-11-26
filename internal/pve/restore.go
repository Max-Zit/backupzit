package pve

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/max-zit/backupzit/internal/diskimg"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/vmlock"
)

// RestoreOptions control a guest restore.
type RestoreOptions struct {
	// VMID selects the guest in the snapshot.
	VMID int
	// NewVMID is the ID of the restored guest: 0 keeps the original ID,
	// a negative value takes the next free ID.
	NewVMID int
	// Name renames the restored guest ("" keeps the name).
	Name string
	// Storage puts all disks on this storage ("" = original storage).
	Storage string
	// Overwrite replaces an existing guest with the same ID (it is stopped
	// and deleted together with its disks). Its disks are deleted only
	// after all disks of the backup are written (C1, 0.59), unless
	// RemoveFirst: then it is deleted before, as before 0.59, which needs
	// room for one copy only but leaves no guest if the restore fails.
	Overwrite   bool
	RemoveFirst bool
	// Start starts the guest after the restore.
	Start    bool
	Progress func(done, total uint64)
	Log      func(msg string, args ...any)
}

// RestoreResult describes the restored guest.
type RestoreResult struct {
	VMID    int      `json:"vmid"`
	Type    string   `json:"type"`
	Name    string   `json:"name,omitempty"`
	Node    string   `json:"node"`
	Disks   []string `json:"disks"`
	Bytes   uint64   `json:"bytes"`
	Started bool     `json:"started,omitempty"`
	// NewMACs is true when network cards got new MAC addresses because
	// the original guest still exists.
	NewMACs bool     `json:"new_macs,omitempty"`
	Notes   []string `json:"notes,omitempty"`
	// LeftOut are the settings an isolated VM was started without (A9).
	LeftOut []string `json:"left_out,omitempty"`
}

// FindGuest returns a guest of a snapshot.
func FindGuest(sn *repo.Snapshot, vmid int) (*repo.Guest, error) {
	for i := range sn.Guests {
		if sn.Guests[i].VMID == vmid {
			return &sn.Guests[i], nil
		}
	}
	if len(sn.Guests) == 0 {
		return nil, fmt.Errorf("snapshot %s is not a Proxmox backup", sn.ID.Short())
	}
	return nil, fmt.Errorf("snapshot %s does not contain guest %d", sn.ID.Short(), vmid)
}

// NextID returns the next free guest ID of the cluster.
func NextID(ctx context.Context) (int, error) {
	out, err := command(ctx, "pvesh", "get", "/cluster/nextid")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.Trim(strings.TrimSpace(string(out)), `"`))
}

// Restore recreates a guest from a snapshot on this node.
func Restore(ctx context.Context, r *repo.Repository, sn *repo.Snapshot, opts RestoreOptions) (*RestoreResult, error) {
	if opts.Log == nil {
		opts.Log = func(string, ...any) {}
	}
	g, err := FindGuest(sn, opts.VMID)
	if err != nil {
		return nil, err
	}
	target := opts.NewVMID
	switch {
	case target == 0:
		target = g.VMID
	case target < 0:
		if target, err = NextID(ctx); err != nil {
			return nil, fmt.Errorf("find a free ID: %w", err)
		}
	}
	if target < 100 || target > 999999999 {
		return nil, fmt.Errorf("invalid guest ID %d", target)
	}
	// A restore over an existing guest stops and deletes it: not while a
	// backup of that guest on this node holds its snapshot (M-13). The
	// guest list is read after the wait.
	unlockVM, err := vmlock.Lock(ctx, vmLockKey(target))
	if err != nil {
		return nil, err
	}
	defer unlockVM()
	inv, err := GetInventory(ctx)
	if err != nil {
		return nil, fmt.Errorf("list guests: %w", err)
	}
	stores, err := readStorage()
	if err != nil {
		return nil, fmt.Errorf("read storage configuration: %w", err)
	}
	res := &RestoreResult{VMID: target, Type: g.Type, Name: g.Name, Node: inv.Node}

	// An existing guest with the target ID.
	var old *Guest
	for _, x := range inv.Guests {
		// An ID from nextid is free; the cluster list can still show a guest just removed.
		if x.VMID != target || opts.NewVMID < 0 {
			continue
		}
		if !opts.Overwrite {
			return nil, fmt.Errorf("guest %d already exists; restore with another ID or choose to overwrite it", target)
		}
		if x.Node != inv.Node {
			return nil, fmt.Errorf("guest %d is on node %s; overwrite it from the agent on that node", target, x.Node)
		}
		if x.Type != g.Type {
			return nil, fmt.Errorf("guest %d is a %s, the backup is a %s", target, x.Type, g.Type)
		}
		old = &x
	}
	tool := guestTool(g.Type)
	id := strconv.Itoa(target)
	if old != nil && opts.RemoveFirst {
		// As before 0.59: room for one copy only, but no guest if the
		// restore fails.
		opts.Log("removing existing guest before the restore", "vmid", target)
		if guestRunning(ctx, *old) {
			if _, err := command(ctx, tool, "stop", id); err != nil {
				return nil, fmt.Errorf("stop guest %d: %w", target, err)
			}
		}
		if _, err := command(ctx, tool, "destroy", id); err != nil {
			return nil, fmt.Errorf("delete guest %d: %w", target, err)
		}
		res.Notes = append(res.Notes, fmt.Sprintf("existing guest %d was removed before the restore and replaced", target))
		old = nil
	}
	// The original still exists: the restore is a second copy.
	clone := false
	if target != g.VMID {
		for _, x := range inv.Guests {
			clone = clone || x.VMID == g.VMID
		}
	}

	content := "images"
	if g.Type == "lxc" {
		content = "rootdir"
	}
	storageFor := func(d repo.GuestDisk) (storage, error) {
		id := d.Storage
		if opts.Storage != "" {
			id = opts.Storage
		}
		st, ok := stores[id]
		if !ok || !st.usable(inv.Node) {
			return st, fmt.Errorf("storage %q is not available on node %s; choose another storage", id, inv.Node)
		}
		if !st.has(content) {
			return st, fmt.Errorf("storage %q does not hold %s", id, map[string]string{"images": "VM disks", "rootdir": "container volumes"}[content])
		}
		switch {
		case d.Files() && st.Type != "zfspool":
			return st, fmt.Errorf("%s was backed up as files from a ZFS subvolume; restore it to a ZFS storage (%s is %s storage)", d.Key, id, st.Type)
		case !d.Files() && g.Type == "lxc" && st.Type == "zfspool":
			return st, fmt.Errorf("%s was backed up as a disk image; restoring it to ZFS storage (%s) is not supported yet, choose another storage", d.Key, id)
		}
		return st, nil
	}

	var total uint64
	for _, d := range g.Disks {
		if _, err := storageFor(d); err != nil {
			return nil, err
		}
		if d.Files() {
			total += d.Bytes
		} else {
			total += d.Size
		}
	}
	// The new disks are written next to the existing guest's: on thin
	// storage (LVM-thin, ZFS, sparse files) a pool that runs full stops the
	// writes of every guest on it, so the room is checked first.
	if old != nil {
		need := map[string]uint64{}
		for _, d := range g.Disks {
			st, _ := storageFor(d)
			need[st.ID] += dataBytes(sn, d)
		}
		for id, n := range need {
			total, avail, err := storageSpace(ctx, id)
			if err != nil {
				opts.Log("free space of the storage not checked", "storage", id, "error", err)
				continue
			}
			reserve := max(total/20, 1<<30)
			if avail < n+reserve {
				return nil, fmt.Errorf("storage %s has %s free: the restore writes about %s next to the disks of the existing guest %d and would leave less than %s; free space, restore to another storage or as a new guest, or choose to delete the existing guest first (the guest was not changed)",
					id, gib(avail), gib(n), target, gib(reserve))
			}
		}
	}

	var done uint64
	progress := func(n uint64) {
		done += n
		if opts.Progress != nil {
			opts.Progress(done, max(total, done))
		}
	}

	// Allocate and fill the disks; remove them again if anything fails -
	// unless the existing guest was deleted meanwhile: then they are the
	// only copy and stay, and the error names them.
	var allocated []string
	ok, keep := false, false
	defer func() {
		if ok || keep {
			return
		}
		c, cancel := bg()
		defer cancel()
		for _, v := range allocated {
			_, _ = command(c, "pvesm", "free", v)
		}
	}()
	// While the disks are written the existing guest stays as it is (and
	// keeps running): a failure leaves it unchanged.
	unchanged := func(err error) error {
		if old == nil {
			return err
		}
		hint := ""
		if s := strings.ToLower(err.Error()); strings.Contains(s, "space") || strings.Contains(s, "enospc") || strings.Contains(s, "full") {
			hint = "; if the storage has no room for both, choose to remove the existing guest first"
		}
		return fmt.Errorf("%w (the existing guest %d was not changed%s)", err, target, hint)
	}
	newVol := map[string]string{} // disk key -> new volume ID
	for _, d := range g.Disks {
		st, _ := storageFor(d)
		if d.Files() {
			volid, err := allocSubvol(ctx, st, target, d.Size)
			if err != nil {
				return nil, unchanged(fmt.Errorf("%s: allocate a subvolume on %s: %w", d.Key, st.ID, err))
			}
			allocated = append(allocated, volid)
			newVol[d.Key] = volid
			opts.Log("restoring volume files", "key", d.Key, "volume", volid)
			if err := writeVolumeFilesFn(ctx, r, sn, d, volid, progress); err != nil {
				return nil, unchanged(fmt.Errorf("%s: restore files to %s: %w", d.Key, volid, err))
			}
			res.Disks = append(res.Disks, d.Key+" → "+volid)
			res.Bytes += d.Bytes
			continue
		}
		volid, err := allocDisk(ctx, st, target, d.Size)
		if err != nil {
			return nil, unchanged(fmt.Errorf("%s: allocate %s on %s: %w", d.Key, formatSize(d.Size), st.ID, err))
		}
		allocated = append(allocated, volid)
		newVol[d.Key] = volid
		opts.Log("writing disk", "key", d.Key, "volume", volid)
		if err := writeDiskFn(ctx, r, sn, d, st, volid, progress); err != nil {
			return nil, unchanged(fmt.Errorf("%s: write %s: %w", d.Key, volid, err))
		}
		res.Disks = append(res.Disks, d.Key+" → "+volid)
		res.Bytes += d.Size
	}
	if err := ctx.Err(); err != nil {
		return nil, unchanged(err)
	}

	// All data is restored: now the existing guest goes. destroy keeps the
	// new volumes, which carry the same ID but are not in its configuration.
	if old != nil {
		opts.Log("replacing existing guest", "vmid", target)
		stopped := false
		// Its own tool says whether it runs now: the cluster list read at the
		// start lags some seconds (a guest started just before the restore).
		if guestRunning(ctx, *old) {
			if _, err := command(ctx, tool, "stop", id); err != nil {
				return nil, unchanged(fmt.Errorf("stop guest %d: %w", target, err))
			}
			stopped = true
		}
		if _, err := command(ctx, tool, "destroy", id, "--destroy-unreferenced-disks", "0"); err != nil {
			if stopped {
				c, cancel := bg()
				_, serr := command(c, tool, "start", id)
				cancel()
				if serr != nil {
					return nil, fmt.Errorf("delete guest %d: %w (it was stopped for the restore and could not be started again: %v; the restored disks were removed)", target, err, serr)
				}
			}
			return nil, unchanged(fmt.Errorf("delete guest %d: %w", target, err))
		}
		keep = true
		res.Notes = append(res.Notes, fmt.Sprintf("existing guest %d was replaced", target))
	}
	kept := func(err error) error {
		if !keep {
			return err
		}
		return fmt.Errorf("%w (the guest it replaced is deleted; the restored disks of ID %d are kept: %s - attach them to a new guest %d, or remove them with pvesm free)", err, target, strings.Join(allocated, ", "), target)
	}

	conf, cloudInit, notes := restoreConfig(g, newVol, opts.Name, clone)
	res.Notes = append(res.Notes, notes...)
	res.NewMACs = clone
	if opts.Name != "" {
		res.Name = opts.Name
	}
	dir := "qemu-server"
	if g.Type == "lxc" {
		dir = "lxc"
	}
	cfgFile := filepath.Join(ConfigDir, "nodes", inv.Node, dir, strconv.Itoa(target)+".conf")
	f, err := os.OpenFile(cfgFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640) // #nosec G302 G304 -- Proxmox guest configuration, 0640 as Proxmox writes it
	if err != nil {
		return nil, kept(fmt.Errorf("create configuration: %w", err))
	}
	if _, err := f.WriteString(conf); err != nil {
		_ = f.Close()
		_ = os.Remove(cfgFile)
		return nil, kept(fmt.Errorf("write configuration: %w", err))
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(cfgFile)
		return nil, kept(fmt.Errorf("write configuration: %w", err))
	}
	ok = true // the disks now belong to the guest

	for key, opt := range cloudInit {
		st := opts.Storage
		if st == "" {
			st = opt
		}
		if _, err := command(ctx, "qm", "set", strconv.Itoa(target), "--"+key, st+":cloudinit"); err != nil {
			res.Notes = append(res.Notes, fmt.Sprintf("cloud-init drive %s not recreated: %v", key, err))
		}
	}
	if opts.Start {
		if _, err := command(ctx, guestTool(g.Type), "start", strconv.Itoa(target)); err != nil {
			res.Notes = append(res.Notes, fmt.Sprintf("guest restored but could not be started: %v", err))
		} else {
			res.Started = true
		}
	}
	return res, nil
}

var createdRe = regexp.MustCompile(`successfully created '([^']+)'`)

// gib formats a size for messages.
func gib(n uint64) string { return strconv.FormatFloat(float64(n)/(1<<30), 'f', 1, 64) + " GiB" }

// dataBytes is about how much a restore writes for disk d: the blocks of
// its image that carry data, or the bytes of its files.
func dataBytes(sn *repo.Snapshot, d repo.GuestDisk) uint64 {
	if d.Files() {
		return d.Bytes
	}
	if d.Image < 0 || d.Image >= len(sn.Images) {
		return d.Size
	}
	var n uint64
	for _, p := range sn.Images[d.Image].Partitions {
		n += p.StoredBytes
	}
	if n == 0 {
		return d.Size
	}
	return n
}

// storageSpace returns the size and free space of a storage in bytes
// ("pvesm status", in KiB).
var storageSpace = func(ctx context.Context, id string) (total, avail uint64, err error) {
	out, err := command(ctx, "pvesm", "status", "--storage", id)
	if err != nil {
		return 0, 0, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 6 || f[0] != id {
			continue
		}
		t, err1 := strconv.ParseUint(f[3], 10, 64)
		a, err2 := strconv.ParseUint(f[5], 10, 64)
		if err1 != nil || err2 != nil {
			break
		}
		return t * 1024, a * 1024, nil
	}
	return 0, 0, fmt.Errorf("unexpected pvesm status output: %s", strings.TrimSpace(string(out)))
}

// The writers of a restore (tests replace them).
var (
	writeDiskFn        = writeDisk
	writeVolumeFilesFn = writeVolumeFiles
)

func allocDisk(ctx context.Context, st storage, vmid int, size uint64) (string, error) {
	kib := (size + 1023) / 1024
	args := []string{"alloc", st.ID, strconv.Itoa(vmid), "", strconv.FormatUint(kib, 10)}
	switch st.Type {
	case "dir", "nfs", "cifs", "glusterfs", "cephfs", "btrfs":
		args = append(args, "--format", "raw")
	}
	out, err := command(ctx, "pvesm", args...)
	if err != nil {
		return "", err
	}
	m := createdRe.FindSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("unexpected pvesm output: %s", strings.TrimSpace(string(out)))
	}
	return string(m[1]), nil
}

// writeDisk writes a disk image to a freshly allocated volume.
func writeDisk(ctx context.Context, r *repo.Repository, sn *repo.Snapshot, d repo.GuestDisk, st storage, volid string, progress func(uint64)) error {
	if d.Image < 0 || d.Image >= len(sn.Images) || len(sn.Images[d.Image].Partitions) != 1 {
		return errors.New("disk image missing in the snapshot")
	}
	path, cleanup, err := volumeDevice(ctx, st, volid)
	if err != nil {
		return err
	}
	defer cleanup()
	// Thin and new file volumes read as zeros; thick LVM may hold old data.
	writeZeros := st.Type == "lvm" || st.Type == "iscsidirect" || st.Type == "iscsi"
	f, err := os.OpenFile(path, os.O_WRONLY, 0) // #nosec G304 -- Proxmox VE configuration files and the agent's work files
	if err != nil {
		return err
	}
	defer f.Close()
	if err := diskimg.Write(ctx, r, &sn.Images[d.Image], f, writeZeros, progress); err != nil {
		return err
	}
	return f.Sync()
}

var (
	qemuNetRe = regexp.MustCompile(`^net\d+$`)
	macRe     = regexp.MustCompile(`(?i)([0-9a-f]{2}:){5}[0-9a-f]{2}`)
)

// restoreConfig rewrites a guest configuration for its new disks. It
// returns the configuration, the cloud-init drives to recreate (key ->
// original storage) and notes for the user.
func restoreConfig(g *repo.Guest, newVol map[string]string, name string, clone bool) (string, map[string]string, []string) {
	var b strings.Builder
	var notes []string
	cloudInit := map[string]string{}
	disks := map[string]repo.GuestDisk{}
	for _, d := range g.Disks {
		disks[d.Key] = d
	}
	re := qemuDiskKey
	if g.Type == "lxc" {
		re = lxcDiskKey
	}
	var hostKeys []string
	for _, line := range strings.Split(strings.TrimRight(g.Config, "\n"), "\n") {
		// Snapshot and pending sections describe the original's own
		// snapshots and volumes: not part of the restored guest.
		if strings.HasPrefix(strings.TrimSpace(line), "[") {
			break
		}
		k, v, found := strings.Cut(line, ":")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !found {
			b.WriteString(line + "\n")
			continue
		}
		// Unused disks and the snapshot parent belong to the original guest;
		// keeping them would let deleting the copy delete the original's volumes.
		if unusedKey.MatchString(k) || k == "parent" {
			continue
		}
		if actsOnHost(g.Type, k) && !slices.Contains(hostKeys, k) {
			hostKeys = append(hostKeys, k)
		}
		switch {
		case re.MatchString(k):
			vol, opts, _ := strings.Cut(v, ",")
			if nv, ok := newVol[k]; ok {
				if disks[k].Size > 0 || !disks[k].Files() {
					opts = setOpt(opts, "size", formatSize(disks[k].Size))
				}
				if g.Type == "lxc" && strings.HasPrefix(k, "mp") && strings.HasPrefix(vol, "volume=") {
					nv = "volume=" + nv
				}
				line = k + ": " + nv
				if opts != "" {
					line += "," + opts
				}
			} else if strings.Contains(vol, "cloudinit") {
				sid, _, _ := strings.Cut(vol, ":")
				cloudInit[k] = sid
				continue
			} else if vol == "none" || strings.Contains(","+opts+",", ",media=cdrom,") {
				// CD/DVD drives are kept as they are.
			} else {
				notes = append(notes, fmt.Sprintf("%s (%s) was not in the backup and was left out", k, vol))
				continue
			}
		case k == "name" && name != "" && g.Type == "qemu", k == "hostname" && name != "" && g.Type == "lxc":
			line = k + ": " + name
		case k == "vmgenid":
			// A new generation ID tells the guest OS it was restored.
			line = "vmgenid: " + newUUID()
		case clone && qemuNetRe.MatchString(k):
			line = k + ": " + macRe.ReplaceAllStringFunc(v, func(string) string { return newMAC() })
		}
		b.WriteString(line + "\n")
	}
	if len(hostKeys) > 0 {
		// Kept like Proxmox VE's own restore does, but they act on this host:
		// from a backup of another (or a compromised) host they deserve a look.
		notes = append(notes, fmt.Sprintf("kept settings from the backup that act on this host: %s; check them before starting the guest", strings.Join(hostKeys, ", ")))
	}
	return b.String(), cloudInit, notes
}

// actsOnHost reports whether a configuration key runs code on, or gives
// the guest access to, the Proxmox host: hook scripts, raw LXC keys (mounts,
// devices, hooks), raw QEMU arguments and passed-through devices.
func actsOnHost(typ, k string) bool {
	switch {
	case k == "hookscript":
		return true
	case typ == "lxc":
		return strings.HasPrefix(k, "lxc.") || hostDevKey.MatchString(k)
	default:
		return k == "args" || hostPCIKey.MatchString(k) || usbKey.MatchString(k)
	}
}

var (
	hostDevKey = regexp.MustCompile(`^dev\d+$`)
	hostPCIKey = regexp.MustCompile(`^hostpci\d+$`)
	usbKey     = regexp.MustCompile(`^usb\d+$`)
)

var unusedKey = regexp.MustCompile(`^unused\d+$`)

func setOpt(opts, key, val string) string {
	parts := []string{}
	set := false
	for _, o := range strings.Split(opts, ",") {
		if o == "" {
			continue
		}
		if k, _, _ := strings.Cut(o, "="); k == key {
			o, set = key+"="+val, true
		}
		parts = append(parts, o)
	}
	if !set {
		parts = append(parts, key+"="+val)
	}
	return strings.Join(parts, ",")
}

func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// newMAC returns a random address in the Proxmox OUI (BC:24:11).
func newMAC() string {
	var b [3]byte
	rand.Read(b[:])
	return fmt.Sprintf("BC:24:11:%02X:%02X:%02X", b[0], b[1], b[2])
}

// volumeDevice returns the block device or file of a volume (mapping Ceph
// images and activating LVM volumes first) and a function that releases it.
func volumeDevice(ctx context.Context, st storage, volid string) (path string, cleanup func(), err error) {
	_, vol, _ := strings.Cut(volid, ":")
	cleanup = func() {}
	switch st.Type {
	case "rbd":
		args := append([]string{"map"}, rbdArgs(st)...)
		out, err := command(ctx, "rbd", append(args, rbdImage(st, vol))...)
		if err != nil {
			return "", nil, err
		}
		path = strings.TrimSpace(string(out))
		cleanup = func() { c, cancel := bg(); defer cancel(); _, _ = command(c, "rbd", "unmap", path) }
	case "lvm", "lvmthin":
		_, _ = command(ctx, "lvchange", "-ay", st.Props["vgname"]+"/"+vol)
		fallthrough
	default:
		if path, err = volumePath(ctx, volid); err != nil {
			return "", nil, err
		}
		if err := waitPath(ctx, path); err != nil {
			return "", nil, err
		}
	}
	return path, cleanup, nil
}
