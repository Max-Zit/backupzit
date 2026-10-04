package pve

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/backupzit/backupzit/internal/diskimg"
	"github.com/backupzit/backupzit/internal/repo"
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
	// and deleted together with its disks).
	Overwrite bool
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
	inv, err := GetInventory(ctx)
	if err != nil {
		return nil, fmt.Errorf("list guests: %w", err)
	}
	stores, err := readStorage()
	if err != nil {
		return nil, fmt.Errorf("read storage configuration: %w", err)
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
	res := &RestoreResult{VMID: target, Type: g.Type, Name: g.Name, Node: inv.Node}

	// An existing guest with the target ID.
	for _, x := range inv.Guests {
		if x.VMID != target {
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
		opts.Log("removing existing guest", "vmid", target)
		tool := guestTool(g.Type)
		if x.Status == "running" {
			if _, err := command(ctx, tool, "stop", strconv.Itoa(target)); err != nil {
				return nil, fmt.Errorf("stop guest %d: %w", target, err)
			}
		}
		if _, err := command(ctx, tool, "destroy", strconv.Itoa(target)); err != nil {
			return nil, fmt.Errorf("delete guest %d: %w", target, err)
		}
		res.Notes = append(res.Notes, fmt.Sprintf("existing guest %d was replaced", target))
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
		if g.Type == "lxc" && st.Type == "zfspool" {
			return st, fmt.Errorf("containers on ZFS storage (%s) are not supported yet; choose another storage", id)
		}
		return st, nil
	}

	var total uint64
	for _, d := range g.Disks {
		if _, err := storageFor(d); err != nil {
			return nil, err
		}
		total += d.Size
	}
	var done uint64
	progress := func(n uint64) {
		done += n
		if opts.Progress != nil {
			opts.Progress(done, max(total, done))
		}
	}

	// Allocate and fill the disks; remove them again if anything fails.
	var allocated []string
	ok := false
	defer func() {
		if ok {
			return
		}
		c, cancel := bg()
		defer cancel()
		for _, v := range allocated {
			command(c, "pvesm", "free", v)
		}
	}()
	newVol := map[string]string{} // disk key -> new volume ID
	for _, d := range g.Disks {
		st, _ := storageFor(d)
		volid, err := allocDisk(ctx, st, target, d.Size)
		if err != nil {
			return nil, fmt.Errorf("%s: allocate %s on %s: %w", d.Key, formatSize(d.Size), st.ID, err)
		}
		allocated = append(allocated, volid)
		newVol[d.Key] = volid
		opts.Log("writing disk", "key", d.Key, "volume", volid)
		if err := writeDisk(ctx, r, sn, d, st, volid, progress); err != nil {
			return nil, fmt.Errorf("%s: write %s: %w", d.Key, volid, err)
		}
		res.Disks = append(res.Disks, d.Key+" → "+volid)
		res.Bytes += d.Size
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
	f, err := os.OpenFile(cfgFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, fmt.Errorf("create configuration: %w", err)
	}
	if _, err := f.WriteString(conf); err != nil {
		f.Close()
		os.Remove(cfgFile)
		return nil, fmt.Errorf("write configuration: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(cfgFile)
		return nil, fmt.Errorf("write configuration: %w", err)
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
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
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
		switch {
		case re.MatchString(k):
			vol, opts, _ := strings.Cut(v, ",")
			if nv, ok := newVol[k]; ok {
				opts = setOpt(opts, "size", formatSize(disks[k].Size))
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
	return b.String(), cloudInit, notes
}

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
		cleanup = func() { c, cancel := bg(); defer cancel(); command(c, "rbd", "unmap", path) }
	case "lvm", "lvmthin":
		command(ctx, "lvchange", "-ay", st.Props["vgname"]+"/"+vol)
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
