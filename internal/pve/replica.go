package pve

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/backupzit/backupzit/internal/diskimg"
	"github.com/backupzit/backupzit/internal/repo"
)

// Replication keeps a stopped copy of a guest (the replica) up to date from
// its backups: the first time the replica is restored like any guest, later
// only the blocks that changed since the backup it holds are written. In a
// disaster the replica is started instead of restoring.

// ReplicaOptions update the replica of one guest.
type ReplicaOptions struct {
	VMID        int    // guest in the backup
	ReplicaVMID int    // 0: create the replica with the next free ID
	Storage     string // for new replicas; "" = the original storage
	// Prev is the backup the replica holds (nil: unknown, write all).
	Prev     *repo.Snapshot
	Log      func(msg string, args ...any)
	Progress func(done, total uint64)
}

// ReplicaResult reports one replica.
type ReplicaResult struct {
	VMID        int      `json:"vmid"`
	ReplicaVMID int      `json:"replica_vmid"`
	Name        string   `json:"name"`
	Created     bool     `json:"created,omitempty"`
	Written     uint64   `json:"written"`
	Notes       []string `json:"notes,omitempty"`
}

func replicaMarker(vmid int) string { return fmt.Sprintf("BackupZit replica of guest %d", vmid) }

// Replicate creates or updates the replica of guest opts.VMID from sn.
func Replicate(ctx context.Context, r *repo.Repository, sn *repo.Snapshot, opts ReplicaOptions) (*ReplicaResult, error) {
	if opts.Log == nil {
		opts.Log = func(string, ...any) {}
	}
	g, err := FindGuest(sn, opts.VMID)
	if err != nil {
		return nil, err
	}
	inv, err := GetInventory(ctx)
	if err != nil {
		return nil, err
	}
	res := &ReplicaResult{VMID: g.VMID, ReplicaVMID: opts.ReplicaVMID, Name: g.Name + "-replica"}
	var existing *Guest
	if opts.ReplicaVMID > 0 {
		for i, x := range inv.Guests {
			if x.VMID == opts.ReplicaVMID {
				existing = &inv.Guests[i]
			}
		}
	}
	if existing == nil {
		newID := -1
		if opts.ReplicaVMID > 0 {
			newID = opts.ReplicaVMID // was deleted: create it again with its ID
			res.Notes = append(res.Notes, fmt.Sprintf("replica %d did not exist and was created again", opts.ReplicaVMID))
		}
		opts.Log("creating replica", "vmid", g.VMID)
		rr, err := Restore(ctx, r, sn, RestoreOptions{VMID: g.VMID, NewVMID: newID, Name: res.Name, Storage: opts.Storage, Progress: opts.Progress, Log: opts.Log})
		if err != nil {
			return nil, err
		}
		res.ReplicaVMID, res.Created, res.Written = rr.VMID, true, rr.Bytes
		res.Notes = append(res.Notes, rr.Notes...)
		if err := markReplica(g, rr.VMID, sn); err != nil {
			return res, err
		}
		return res, nil
	}
	if existing.Node != inv.Node {
		return nil, fmt.Errorf("replica %d is on node %s; it is updated by the agent on that node", existing.VMID, existing.Node)
	}
	if existing.Status != "stopped" {
		return nil, fmt.Errorf("replica %d is %s (failed over?); it is not updated while it runs — delete it to start replicating again", existing.VMID, existing.Status)
	}
	p, err := configPath(g.Type, existing.VMID)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	conf := string(b)
	if !strings.Contains(conf, replicaMarker(g.VMID)) {
		return nil, fmt.Errorf("guest %d is not the BackupZit replica of guest %d; it is left alone", existing.VMID, g.VMID)
	}
	if strings.Contains(conf, "\n[") || strings.HasPrefix(conf, "[") {
		return nil, fmt.Errorf("replica %d has snapshots; delete them so that it can be updated", existing.VMID)
	}
	vols := map[string]string{}
	sizes := map[string]uint64{}
	for _, line := range strings.Split(conf, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		vol, o, _ := strings.Cut(v, ",")
		if strings.HasPrefix(vol, "volume=") {
			vol = strings.TrimPrefix(vol, "volume=")
		}
		vols[k] = vol
		for _, x := range strings.Split(o, ",") {
			if sv, ok := strings.CutPrefix(x, "size="); ok {
				sizes[k], _ = parseSize(sv)
			}
		}
	}
	stores, err := readStorage()
	if err != nil {
		return nil, err
	}
	var prevGuest *repo.Guest
	if opts.Prev != nil {
		prevGuest, _ = FindGuest(opts.Prev, g.VMID)
	}
	if prevGuest == nil {
		res.Notes = append(res.Notes, "the backup the replica holds is no longer available: all blocks were compared by writing them")
	}
	var total, done uint64
	for _, d := range g.Disks {
		total += d.Size
	}
	for _, d := range g.Disks {
		volid, ok := vols[d.Key]
		if !ok {
			res.Notes = append(res.Notes, fmt.Sprintf("disk %s was added after the replica was created and is not replicated; delete replica %d to create it again", d.Key, existing.VMID))
			continue
		}
		if d.Image < 0 || d.Image >= len(sn.Images) {
			return res, fmt.Errorf("%s: image missing in the backup", d.Key)
		}
		if cur := sizes[d.Key]; cur > 0 && d.Size > cur {
			opts.Log("growing replica disk", "disk", d.Key, "size", formatSize(d.Size))
			tool := guestTool(g.Type)
			if _, err := command(ctx, tool, "resize", strconv.Itoa(existing.VMID), d.Key, formatSize(d.Size)); err != nil {
				return res, fmt.Errorf("%s: grow to %s: %w", d.Key, formatSize(d.Size), err)
			}
		} else if cur > d.Size {
			return res, fmt.Errorf("%s shrank (%s → %s); delete replica %d to create it again", d.Key, formatSize(cur), formatSize(d.Size), existing.VMID)
		}
		sid, _, _ := strings.Cut(volid, ":")
		st, ok := stores[sid]
		if !ok {
			return res, fmt.Errorf("%s: storage %s of the replica is unknown", d.Key, sid)
		}
		var prevImg *repo.DiskImage
		if prevGuest != nil {
			for _, pd := range prevGuest.Disks {
				if pd.Key == d.Key && pd.Image >= 0 && pd.Image < len(opts.Prev.Images) {
					prevImg = &opts.Prev.Images[pd.Image]
				}
			}
		}
		path, cleanup, err := volumeDevice(ctx, st, volid)
		if err != nil {
			return res, fmt.Errorf("%s: %w", d.Key, err)
		}
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			cleanup()
			return res, err
		}
		opts.Log("updating replica disk", "disk", d.Key, "volume", volid)
		n, err := diskimg.WriteChanges(ctx, r, &sn.Images[d.Image], prevImg, f, func(n uint64) {
			done += n
			if opts.Progress != nil {
				opts.Progress(done, total)
			}
		})
		if err == nil {
			err = f.Sync()
		}
		f.Close()
		cleanup()
		if err != nil {
			return res, fmt.Errorf("%s: %w", d.Key, err)
		}
		res.Written += n
	}
	if err := markReplica(g, existing.VMID, sn); err != nil {
		return res, err
	}
	return res, nil
}

// markReplica describes the replica in its configuration and makes sure it
// never starts with the node.
func markReplica(g *repo.Guest, vmid int, sn *repo.Snapshot) error {
	p, err := configPath(g.Type, vmid)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	var out strings.Builder
	out.WriteString("description: " + replicaMarker(g.VMID) + " (" + g.Name + "), updated from the backup of " +
		sn.Time.Local().Format("2006-01-02 15:04") + ". Start it only if the original is lost; replication pauses while it runs.\n")
	for _, l := range strings.SplitAfter(string(b), "\n") {
		if strings.HasPrefix(l, "description:") || strings.HasPrefix(l, "#") || strings.HasPrefix(l, "onboot:") {
			continue
		}
		out.WriteString(l)
	}
	return os.WriteFile(p, []byte(out.String()), 0o640)
}

// StartReplica starts a replica (failover).
func StartReplica(ctx context.Context, replicaVMID, vmid int) error {
	for _, typ := range []string{"qemu", "lxc"} {
		p, err := configPath(typ, replicaVMID)
		if err != nil {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if !strings.Contains(string(b), replicaMarker(vmid)) {
			return fmt.Errorf("guest %d is not the replica of guest %d", replicaVMID, vmid)
		}
		_, err = command(ctx, guestTool(typ), "start", strconv.Itoa(replicaVMID))
		return err
	}
	return errors.New("replica " + strconv.Itoa(replicaVMID) + " does not exist on this node")
}
