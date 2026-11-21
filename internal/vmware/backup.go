package vmware

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/diskimg"
	"github.com/max-zit/backupzit/internal/leftover"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/vmlock"
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
	// VMStarted and VMFinished (optional) tell which VM the backup works
	// on and the result of each, for the run's progress (0.57).
	VMStarted  func(name string, total int)
	VMFinished func(name string, failed bool)
}

// guestConfig is stored as repo.Guest.Config of VMware guests.
type guestConfig struct {
	VM    VM     `json:"vm"`
	VMX   string `json:"vmx"`
	NVRAM []byte `json:"nvram,omitempty"`
	Host  string `json:"host"`
	// Node is the ESXi host the VM ran on (vCenter).
	Node string `json:"node,omitempty"`
}

// ErrNoVMs: none of the selected VMs is on the host (or the connection).
var ErrNoVMs = errors.New("no virtual machines on this ESXi host")

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
	// Jobs made before vCenter took the host over name VMs by their
	// BIOS UUID.
	for _, v := range vms {
		if _, taken := byID[v.BIOSVMID]; v.BIOSVMID != 0 && !taken {
			byID[v.BIOSVMID] = v
		}
	}
	var sel []VM
	if opts.VMIDs == nil {
		excl := map[int]bool{}
		for _, id := range opts.Exclude {
			excl[id] = true
		}
		for _, v := range vms {
			if !excl[v.VMID] && !(v.BIOSVMID != 0 && excl[v.BIOSVMID]) {
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
		return nil, ErrNoVMs
	}
	prev, err := diskimg.PreviousDisks(ctx, r, "vmware")
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
	ha, haErr := c.hotAddProxy(ctx, opts.Log)
	if haErr != nil {
		addErr("hot-add", fmt.Errorf("%w; the disks of running VMs were copied on the datastore instead", haErr))
	}
	for _, v := range sel {
		opts.Log("backing up VM", "name", v.Name, "uuid", v.UUID)
		if opts.VMStarted != nil {
			opts.VMStarted(v.Name, len(sel))
		}
		finished := func(failed bool) {
			if opts.VMFinished != nil {
				opts.VMFinished(v.Name, failed)
			}
		}
		// A session that ended during the previous VM (a long read) fails
		// only that VM's clean-up at most, not every VM after it (#44).
		n := c.Relogins
		if err := c.ensureSession(ctx); err != nil {
			addErr(v.Name, err)
			stats.Failed = append(stats.Failed, v.Name)
			finished(true)
			continue
		}
		g, warns, err := c.backupVM(ctx, r, v, sn, prev, progress, ha)
		if c.Relogins > n {
			opts.Log("the vSphere session had ended; signed in again", "host", c.conn.Host, "times", c.Relogins-n)
		}
		for _, w := range warns {
			addErr(v.Name, errors.New(w))
		}
		if err != nil {
			if ctx.Err() != nil {
				// A snapshot or disk copy that could not be removed is
				// still told about when the run was cancelled (S-1).
				return nil, leftover.CancelError(ctx.Err(), stats.Errors) // the warnings are in it
			}
			addErr(v.Name, err)
			stats.Failed = append(stats.Failed, v.Name)
			finished(true)
			continue
		}
		sn.Guests = append(sn.Guests, *g)
		sn.Paths = append(sn.Paths, "vmware/"+v.UUID)
		stats.Files++
		finished(false)
	}
	// What earlier runs left on other VMs of the connection (S-2).
	c.cleanAfterBackup(ctx, opts.Log)
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

// vmLockKey names a VM for vmlock: backups and in-place restores of one VM
// through this agent run one at a time.
func (c *Client) vmLockKey(uuid string) string { return "vmware-" + c.conn.Host + "-" + uuid }

func (c *Client) backupVM(ctx context.Context, r *repo.Repository, v VM, sn *repo.Snapshot, prev map[string]diskimg.Previous, progress func(uint64), ha *hotAdd) (_ *repo.Guest, warns []string, _ error) {
	if len(v.Disks) == 0 {
		return nil, nil, errors.New("the VM has no virtual disks")
	}
	// One BackupZit operation per VM on this proxy: the leftover sweep
	// removes every backupzit snapshot and the clone path is shared, so a
	// parallel run of the same VM would lose its snapshot or clone (M-13).
	unlockVM, err := vmlock.Lock(ctx, c.vmLockKey(v.UUID))
	if err != nil {
		return nil, nil, err
	}
	defer unlockVM()
	vmxPath, err := parseDSPath(v.VMX)
	if err != nil {
		return nil, nil, err
	}
	// Configuration first: it is what the restore needs to rebuild the VM.
	vmxData, err := c.get(ctx, vmxPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read configuration: %w", err)
	}
	cfg := guestConfig{VM: v, VMX: string(vmxData), Host: c.conn.Host, Node: v.Host}
	nv := parseVMX(vmxData).Get("nvram")
	if nv == "" {
		nv = strings.TrimSuffix(path.Base(vmxPath.Path), ".vmx") + ".nvram"
	}
	if !strings.HasPrefix(nv, "/") {
		if b, err := c.get(ctx, vmxPath.join(nv)); err == nil {
			cfg.NVRAM = b
		}
	}

	node := c.conn.Host
	if c.VCenter && v.Host != "" {
		node = v.Host
	}
	g := &repo.Guest{Platform: "vmware", Type: "vm", VMID: v.VMID, ID: v.UUID, Name: v.Name, Node: node, Running: v.Power == "poweredOn"}
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
		// Remove snapshots left behind by an interrupted backup (the VM's
		// lock is held: none of them belongs to a running run of this agent).
		if old, err := c.leftovers(ctx, v.Ref); err == nil {
			for _, s := range old {
				it := c.snapshotItem(v, s.Name)
				it.Created = s.Created.UTC()
				leftover.Add(context.Background(), it) // since when, for the console
				if _, err := c.removeSnapshotVerified(ctx, v, s.Name, nil); err != nil {
					warns = append(warns, fmt.Sprintf("the snapshot %s left by an earlier backup could not be removed (%v); the agent tries again and the dashboard lists it until it is gone", s.Name, err))
				}
			}
		}
		// Disks that are still deltas although the VM has no snapshot are
		// what a snapshot removal that did not finish leaves (the session
		// ended during it, #44): consolidate them first, or the backup would
		// refuse the delta disk. A VM with snapshots of its own is never
		// touched; its backup names them.
		if v.hasDelta() {
			if own, err := c.hasSnapshots(ctx, v.Ref); err == nil && !own {
				if note := c.consolidate(ctx, v, "before the backup (an earlier snapshot removal did not finish)"); note != "" {
					warns = append(warns, note)
				}
				if v, err = c.vm(ctx, v.Ref); err != nil {
					return nil, warns, err
				}
			}
		}
		name := snapPrefix + strconv.FormatInt(time.Now().Unix(), 10)
		// Recorded before it exists: an agent crash or a cancel during the
		// creation leaves a record the janitor finds the snapshot by (S-1).
		leftover.Add(ctx, c.snapshotItem(v, name))
		bg := func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.WithoutCancel(ctx), 30*time.Minute)
		}
		quiesce := v.Tools && v.Power == "poweredOn"
		s, err := c.createSnapshot(ctx, v, name, quiesce)
		if err != nil && quiesce && ctx.Err() == nil {
			// A failed quiesced snapshot may still be listed: it goes first,
			// the second one has the same name.
			c2, cancel := bg()
			_, rerr := c.removeSnapshotVerified(c2, v, name, nil)
			cancel()
			if rerr == nil {
				leftover.Add(ctx, c.snapshotItem(v, name))
				warns = append(warns, "quiesced snapshot failed ("+err.Error()+"), took a crash-consistent one")
				s, err = c.createSnapshot(ctx, v, name, false)
			}
		}
		if err != nil || ctx.Err() != nil {
			// Created after all (a cancel, a task that reported an error
			// late), or not: look it up by name and remove it.
			c2, cancel := bg()
			defer cancel()
			if _, rerr := c.removeSnapshotVerified(c2, v, name, leftover.RunWaits); rerr != nil {
				warns = append(warns, fmt.Sprintf("the backup snapshot %s of VM %s could not be removed after the failed creation (%v); the agent tries again and the dashboard lists it until it is gone", name, v.Name, rerr))
			}
			if err == nil {
				err = ctx.Err()
			}
			return nil, warns, fmt.Errorf("create snapshot: %w", err)
		}
		removed := false
		removeSnap := func() {
			if removed {
				return
			}
			removed = true
			c2, cancel := bg()
			defer cancel()
			note, err := c.removeSnapshotVerified(c2, v, name, leftover.RunWaits)
			switch {
			case err != nil:
				warns = append(warns, fmt.Sprintf("the backup snapshot %s of VM %s could not be removed (%v); the agent tries again and the dashboard lists it until it is gone", name, v.Name, err))
			case note != "":
				warns = append(warns, note)
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
		// With hot-add they are read through the agent's VM while the
		// snapshot exists instead; a disk that fails there is copied.
		if v.Power != "poweredOff" {
			defer func() {
				for _, p := range plans {
					if p.clone != nil {
						c2, cancel := bg()
						if err := c.deleteCopy(c2, v, *p.clone); err != nil {
							warns = append(warns, fmt.Sprintf("the temporary disk copy %s could not be deleted (%v); the agent tries again and the dashboard lists it until it is gone", p.clone, err))
						}
						cancel()
					}
				}
				for _, p := range plans {
					if p.clone != nil { // the directory goes once it is empty
						c.removeDirIfEmpty(context.Background(), p.clone.DS, tmpDir)
					}
				}
			}()
			for i := range plans {
				if ha != nil {
					img, err := c.storeDiskHotAdd(ctx, ha, r, v, plans[i], sn, progress)
					if err == nil {
						plans[i].stored = &img
						continue
					}
					if ctx.Err() != nil {
						return nil, warns, ctx.Err()
					}
					warns = append(warns, fmt.Sprintf("%s: hot-add failed, the disk was copied on the datastore instead: %v", plans[i].disk.Key, err))
				}
				if err := c.cloneDisk(ctx, v, &plans[i]); err != nil {
					return nil, warns, fmt.Errorf("%s: %w", plans[i].disk.Key, err)
				}
			}
		}
		removeSnap()
	}
	for _, p := range plans {
		if p.stored != nil {
			g.Disks = append(g.Disks, *p.stored)
			continue
		}
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
	// stored is the disk already read through hot-add.
	stored *repo.GuestDisk
}

// planDisk decides which areas of a disk to read: with changed block
// tracking only what changed since the previous backup, or the allocated
// areas; otherwise the whole disk.
func (c *Client) planDisk(ctx context.Context, v VM, s *snapshot, d Disk, prev map[string]diskimg.Previous) (diskPlan, error) {
	if d.Delta {
		return diskPlan{}, fmt.Errorf("%s is a snapshot delta disk; delete or consolidate the VM's own snapshots before backing it up", d.File)
	}
	p := diskPlan{disk: d, areas: []diskimg.Area{{Offset: 0, Length: d.Capacity}}, method: "whole disk"}
	if d.ChangeID == "" {
		return p, nil
	}
	if pd, ok := prev[diskimg.PreviousKey(v.UUID, d.Key)]; ok && pd.Image.Size == d.Capacity {
		if a, err := c.changedAreas(ctx, v, *s, d, pd.ChangeID); err == nil {
			p.areas, p.base, p.method = a, pd.Image, "changed blocks"
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
	if c.exists(ctx, dst.DS, dst.Path) {
		_ = c.deleteDisk(ctx, dst) // left behind by an interrupted backup
	}
	if err := c.mkdir(ctx, dst.DS, tmpDir); err != nil {
		return fmt.Errorf("create %s on datastore %s: %w", tmpDir, dst.DS, err)
	}
	// Recorded before the copy starts: a crash leaves a record (S-2).
	leftover.Add(ctx, c.copyItem(v, dst))
	if err := c.copyDisk(ctx, src, dst); err != nil {
		// A partial copy takes datastore space: delete it now, also when
		// the run was cancelled.
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer cancel()
		if derr := c.deleteCopy(dctx, v, dst); derr != nil {
			err = fmt.Errorf("%w; the partial copy %s could not be deleted (%v), the agent tries again", err, dst, derr)
		}
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
	return c.storeImage(ctx, r, v, p, sn, progress, &fileReader{ctx: ctx, c: c, url: c.fileURL(flat)}, p.method)
}

// storeDiskHotAdd reads a disk of the snapshot through the agent's VM.
func (c *Client) storeDiskHotAdd(ctx context.Context, ha *hotAdd, r *repo.Repository, v VM, p diskPlan, sn *repo.Snapshot, progress func(uint64)) (repo.GuestDisk, error) {
	dev, release, err := ha.attach(ctx, p.disk.File, p.disk.Capacity)
	if err != nil {
		return repo.GuestDisk{}, err
	}
	defer release()
	gd, err := c.storeImage(ctx, r, v, p, sn, progress, dev, p.method+", hot-add")
	gd.Via = "hot-add"
	return gd, err
}

// storeImage stores the planned areas of a disk read from ra.
func (c *Client) storeImage(ctx context.Context, r *repo.Repository, v VM, p diskPlan, sn *repo.Snapshot, progress func(uint64), ra io.ReaderAt, method string) (repo.GuestDisk, error) {
	src, err := parseDSPath(p.disk.File)
	if err != nil {
		return repo.GuestDisk{}, err
	}
	d := p.disk
	img, err := diskimg.StoreAreas(ctx, r, ra, d.Capacity, p.areas, p.base,
		len(sn.Images), fmt.Sprintf("vmware %s %s %s (%s)", v.Name, d.Key, d.File, method), "snapshot", progress)
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
