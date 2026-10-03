package vmware

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/backupzit/backupzit/internal/diskimg"
	"github.com/backupzit/backupzit/internal/repo"
)

// BackupOptions select the VMs to back up.
type BackupOptions struct {
	// VMIDs lists VMs by their BackupZit number (VMIDFor); nil means all.
	VMIDs    []int
	Exclude  []int
	Hostname string // recorded in the snapshot (default: the ESXi host)
	Version  string
	Tags     []string
	Progress func(done, total uint64)
	Log      func(msg string, args ...any)
}

// guestConfig is stored as repo.Guest.Config of VMware guests.
type guestConfig struct {
	VM    VM     `json:"vm"`
	VMX   string `json:"vmx"`
	NVRAM []byte `json:"nvram,omitempty"`
	Host  string `json:"host"`
}

// prevDisk is the latest backup of a disk, for changed block backups.
type prevDisk struct {
	img      *repo.DiskImage
	changeID string
}

// previousDisks finds, per VM UUID and disk key, the newest backup of that
// disk in the repository that recorded a change ID.
func previousDisks(ctx context.Context, r *repo.Repository) (map[string]prevDisk, error) {
	snaps, err := r.ListSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]prevDisk{}
	for i := len(snaps) - 1; i >= 0; i-- { // newest first
		sn := snaps[i]
		for _, g := range sn.Guests {
			if g.Platform != "vmware" {
				continue
			}
			for _, d := range g.Disks {
				k := strings.ToLower(g.ID) + "/" + d.Key
				if _, seen := out[k]; seen || d.ChangeID == "" || d.Image < 0 || d.Image >= len(sn.Images) {
					continue
				}
				img := sn.Images[d.Image]
				out[k] = prevDisk{img: &img, changeID: d.ChangeID}
			}
		}
	}
	return out, nil
}

// Backup snapshots the selected VMs and stores their disks and
// configuration in one repository snapshot.
func (c *Client) Backup(ctx context.Context, r *repo.Repository, opts BackupOptions) (*repo.Snapshot, error) {
	if opts.Log == nil {
		opts.Log = func(string, ...any) {}
	}
	vms, err := c.VMs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list VMs: %w", err)
	}
	if opts.Hostname == "" {
		opts.Hostname = c.conn.Host
	}
	var stats repo.SnapshotStats
	addErr := func(item string, err error) { stats.Errors = append(stats.Errors, fmt.Sprintf("%s: %v", item, err)) }
	byID := map[int]VM{}
	for _, v := range vms {
		byID[v.VMID] = v
	}
	var sel []VM
	if opts.VMIDs == nil {
		excl := map[int]bool{}
		for _, id := range opts.Exclude {
			excl[id] = true
		}
		for _, v := range vms {
			if !excl[v.VMID] {
				sel = append(sel, v)
			}
		}
	} else {
		for _, id := range opts.VMIDs {
			v, ok := byID[id]
			if !ok {
				addErr(fmt.Sprintf("VM %d", id), errors.New("does not exist on this host (any more)"))
				continue
			}
			sel = append(sel, v)
		}
	}
	if len(sel) == 0 && len(stats.Errors) == 0 {
		return nil, errors.New("no virtual machines on this ESXi host")
	}
	prev, err := previousDisks(ctx, r)
	if err != nil {
		return nil, fmt.Errorf("read earlier backups: %w", err)
	}
	var total, done uint64
	for _, v := range sel {
		total += v.DiskSize()
	}
	progress := func(n uint64) {
		done += n
		if opts.Progress != nil {
			opts.Progress(done, max(total, done))
		}
	}
	start := time.Now()
	before := r.Stats()
	sn := &repo.Snapshot{Time: start.UTC(), Hostname: opts.Hostname, Tags: append([]string{"vmware"}, opts.Tags...), ProgramVersion: opts.Version}
	for _, v := range sel {
		opts.Log("backing up VM", "name", v.Name, "uuid", v.UUID)
		g, warns, err := c.backupVM(ctx, r, v, sn, prev, progress)
		for _, w := range warns {
			addErr(v.Name, errors.New(w))
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			addErr(v.Name, err)
			continue
		}
		sn.Guests = append(sn.Guests, *g)
		sn.Paths = append(sn.Paths, "vmware/"+v.UUID)
		stats.Files++
	}
	if len(sn.Guests) == 0 {
		return nil, fmt.Errorf("no VM could be backed up: %s", strings.Join(stats.Errors, "; "))
	}
	for _, img := range sn.Images {
		stats.Bytes += img.Partitions[0].StoredBytes
	}
	stats.BytesRead = done
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
	sn.Tree, sn.Stats = treeID, stats
	if _, err := r.SaveSnapshot(ctx, sn); err != nil {
		return nil, fmt.Errorf("save snapshot: %w", err)
	}
	return sn, nil
}

func (c *Client) backupVM(ctx context.Context, r *repo.Repository, v VM, sn *repo.Snapshot, prev map[string]prevDisk, progress func(uint64)) (_ *repo.Guest, warns []string, _ error) {
	if len(v.Disks) == 0 {
		return nil, nil, errors.New("the VM has no virtual disks")
	}
	vmxPath, err := parseDSPath(v.VMX)
	if err != nil {
		return nil, nil, err
	}
	// Configuration first: it is what the restore needs to rebuild the VM.
	vmxData, err := c.get(ctx, vmxPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read configuration: %w", err)
	}
	cfg := guestConfig{VM: v, VMX: string(vmxData), Host: c.conn.Host}
	nv := parseVMX(vmxData).Get("nvram")
	if nv == "" {
		nv = strings.TrimSuffix(path.Base(vmxPath.Path), ".vmx") + ".nvram"
	}
	if !strings.HasPrefix(nv, "/") {
		if b, err := c.get(ctx, vmxPath.join(nv)); err == nil {
			cfg.NVRAM = b
		}
	}

	g := &repo.Guest{Platform: "vmware", Type: "vm", VMID: v.VMID, ID: v.UUID, Name: v.Name, Node: c.conn.Host, Running: v.Power == "poweredOn"}
	var plans []diskPlan
	if v.Template {
		g.Consistency = "template, disks read directly"
		for _, d := range v.Disks {
			plans = append(plans, diskPlan{disk: d, areas: []diskimg.Area{{Offset: 0, Length: d.Capacity}}, method: "whole disk"})
		}
	} else {
		note, err := c.enableCBT(ctx, v)
		if err != nil {
			warns = append(warns, "could not enable changed block tracking: "+err.Error())
		} else if note != "" {
			warns = append(warns, note)
		}
		if v, err = c.vm(ctx, v.Ref); err != nil {
			return nil, warns, err
		}
		// Remove snapshots left behind by an interrupted backup.
		if old, err := c.leftovers(ctx, v.Ref); err == nil {
			for _, s := range old {
				c.removeSnapshot(ctx, v, s)
			}
		}
		name := snapPrefix + strconv.FormatInt(time.Now().Unix(), 10)
		quiesce := v.Tools && v.Power == "poweredOn"
		s, err := c.createSnapshot(ctx, v, name, quiesce)
		if err != nil && quiesce {
			warns = append(warns, "quiesced snapshot failed ("+err.Error()+"), took a crash-consistent one")
			s, err = c.createSnapshot(ctx, v, name, false)
		}
		if err != nil {
			return nil, warns, fmt.Errorf("create snapshot: %w", err)
		}
		removed := false
		removeSnap := func() {
			if removed {
				return
			}
			removed = true
			c2, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			if err := c.removeSnapshot(c2, v, *s); err != nil {
				warns = append(warns, "remove snapshot "+name+": "+err.Error())
			}
		}
		defer removeSnap()
		switch {
		case v.Power != "poweredOn":
			g.Consistency = "VM was off, disks read from a snapshot"
		case s.Quiesced:
			g.Consistency = "snapshot, guest quiesced by VMware Tools (application-consistent)"
		default:
			g.Consistency = "snapshot, crash-consistent (VMware Tools not running)"
		}
		disks, err := c.snapshotDisks(ctx, *s)
		if err != nil {
			return nil, warns, err
		}
		for _, d := range disks {
			p, err := c.planDisk(ctx, v, s, d, prev)
			if err != nil {
				return nil, warns, fmt.Errorf("%s: %w", d.Key, err)
			}
			plans = append(plans, p)
		}
		// A running VM keeps its base disks locked while the snapshot
		// exists: copy them on the datastore, then drop the snapshot at once.
		if v.Power != "poweredOff" {
			defer func() {
				for _, p := range plans {
					if p.clone != nil {
						c.run(context.Background(), "vmkfstools -U "+sq(p.clone.local()))
					}
				}
			}()
			for i := range plans {
				if err := c.cloneDisk(ctx, v, &plans[i]); err != nil {
					return nil, warns, fmt.Errorf("%s: %w", plans[i].disk.Key, err)
				}
			}
		}
		removeSnap()
	}
	for _, p := range plans {
		img, err := c.storeDisk(ctx, r, v, p, sn, progress)
		if err != nil {
			return nil, warns, fmt.Errorf("%s: %w", p.disk.Key, err)
		}
		g.Disks = append(g.Disks, img)
	}
	b, _ := json.Marshal(cfg)
	g.Config = string(b)
	return g, warns, nil
}

// diskPlan is what to read of one disk.
type diskPlan struct {
	disk   Disk
	areas  []diskimg.Area
	base   *repo.DiskImage
	method string
	// clone is a temporary copy of the disk read instead of the original.
	clone *dsPath
}

// planDisk decides which areas of a disk to read: with changed block
// tracking only what changed since the previous backup, or the allocated
// areas; otherwise the whole disk.
func (c *Client) planDisk(ctx context.Context, v VM, s *snapshot, d Disk, prev map[string]prevDisk) (diskPlan, error) {
	if d.Delta {
		return diskPlan{}, fmt.Errorf("%s is a snapshot delta disk; delete or consolidate the VM's own snapshots before backing it up", d.File)
	}
	p := diskPlan{disk: d, areas: []diskimg.Area{{Offset: 0, Length: d.Capacity}}, method: "whole disk"}
	if d.ChangeID == "" {
		return p, nil
	}
	if pd, ok := prev[strings.ToLower(v.UUID)+"/"+d.Key]; ok && pd.img.Size == d.Capacity {
		if a, err := c.changedAreas(ctx, v, *s, d, pd.changeID); err == nil {
			p.areas, p.base, p.method = a, pd.img, "changed blocks"
			return p, nil
		}
	}
	if a, err := c.changedAreas(ctx, v, *s, d, "*"); err == nil {
		p.areas, p.method = a, "allocated blocks"
	}
	return p, nil
}

const tmpDir = "backupzit-tmp"

// cloneDisk copies a disk (as of the snapshot) to a thin temporary disk on
// the same datastore.
func (c *Client) cloneDisk(ctx context.Context, v VM, p *diskPlan) error {
	src, err := parseDSPath(p.disk.File)
	if err != nil {
		return err
	}
	dst := dsPath{DS: src.DS, Path: fmt.Sprintf("%s/%s-%s.vmdk", tmpDir, strings.ToLower(v.UUID), strings.ReplaceAll(p.disk.Key, ":", "-"))}
	dir := "/vmfs/volumes/" + dst.DS + "/" + tmpDir
	c.run(ctx, "vmkfstools -U "+sq(dst.local())+" 2>/dev/null")
	if _, err := c.run(ctx, "mkdir -p "+sq(dir)+" && vmkfstools -i "+sq(src.local())+" -d thin "+sq(dst.local())); err != nil {
		return fmt.Errorf("copy the disk on the datastore (needs free space for its used blocks): %w", err)
	}
	p.clone = &dst
	return nil
}

// storeDisk reads the planned areas of a disk into the repository.
func (c *Client) storeDisk(ctx context.Context, r *repo.Repository, v VM, p diskPlan, sn *repo.Snapshot, progress func(uint64)) (repo.GuestDisk, error) {
	src, err := parseDSPath(p.disk.File)
	if err != nil {
		return repo.GuestDisk{}, err
	}
	read := src
	if p.clone != nil {
		read = *p.clone
	}
	flat, err := c.flatFile(ctx, read)
	if err != nil {
		return repo.GuestDisk{}, err
	}
	d := p.disk
	img, err := diskimg.StoreAreas(ctx, r, &fileReader{ctx: ctx, c: c, url: c.fileURL(flat)}, d.Capacity, p.areas, p.base,
		len(sn.Images), fmt.Sprintf("vmware %s %s %s (%s)", v.Name, d.Key, d.File, p.method), "snapshot", progress)
	if err != nil {
		return repo.GuestDisk{}, err
	}
	sn.Images = append(sn.Images, img)
	format := "thick"
	if d.Thin {
		format = "thin"
	}
	return repo.GuestDisk{Key: d.Key, Volume: d.File, Storage: src.DS, Format: format, Size: d.Capacity,
		Image: len(sn.Images) - 1, ChangeID: d.ChangeID}, nil
}
