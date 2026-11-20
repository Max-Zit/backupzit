package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/diskimg"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/vmlock"
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
	for _, d := range g.Disks {
		if d.Path != "" {
			// Updating a replica compares disk blocks; these volumes are files.
			return nil, fmt.Errorf("container %d is backed up as files (ZFS subvolume %s): replicas of such containers are not supported yet; restore it instead", g.VMID, d.Key)
		}
	}
	// The update writes the replica's disks: not while a backup of the
	// replica (a job over all guests) holds its snapshot (M-13). Taken
	// before the guest list is read, so a replica started during the wait
	// is seen as running.
	unlockVM := func() {}
	defer func() { unlockVM() }()
	if opts.ReplicaVMID > 0 {
		unlock, err := vmlock.Lock(ctx, vmLockKey(opts.ReplicaVMID))
		if err != nil {
			return nil, err
		}
		unlockVM = unlock
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
		// Restore locks the new guest itself (vmlock is not reentrant).
		unlockVM()
		unlockVM = func() {}
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
	b, err := os.ReadFile(p) // #nosec G304 -- Proxmox VE configuration files and the agent's work files
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
	} else if why := replicaDiverged(ctx, conf, opts.Prev.ID.String(), g.Type, existing.VMID); why != "" {
		opts.Log("comparing all blocks of the replica", "replica", existing.VMID, "why", why)
		prevGuest = nil
		res.Notes = append(res.Notes, why+": all blocks were compared by writing them")
	}
	// The state token says the disks hold the backup named in it. It goes
	// before the first write and comes back (markReplica) only once every
	// disk is written: an update cut short by an agent or node crash leaves
	// disks that hold parts of two backups, and the next update must then
	// compare every block (L-N21).
	if stateToken.MatchString(conf) {
		if err := os.WriteFile(p, []byte(stateToken.ReplaceAllString(conf, "")), 0o640); err != nil { // #nosec G306 G703 -- p is configPath: the Proxmox folder and a guest number; 0640 as Proxmox writes it
			return res, fmt.Errorf("replica %d: clear its state before the update: %w", existing.VMID, err)
		}
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
		f, err := os.OpenFile(path, os.O_WRONLY, 0) // #nosec G304 -- Proxmox VE configuration files and the agent's work files
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
		_ = f.Close()
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
	b, err := os.ReadFile(p) // #nosec G304 -- Proxmox VE configuration files and the agent's work files
	if err != nil {
		return err
	}
	var out strings.Builder
	out.WriteString("description: " + replicaMarker(g.VMID) + " (" + g.Name + "), updated from the backup of " +
		sn.Time.Local().Format("2006-01-02 15:04") + ". Start it only if the original is lost; replication pauses while it runs. " +
		replicaState(sn.ID.String(), time.Now()) + "\n")
	for _, l := range strings.SplitAfter(string(b), "\n") {
		if strings.HasPrefix(l, "description:") || strings.HasPrefix(l, "#") || strings.HasPrefix(l, "onboot:") {
			continue
		}
		out.WriteString(l)
	}
	return os.WriteFile(p, []byte(out.String()), 0o640) // #nosec G306 -- Proxmox guest configuration, 0640 as Proxmox writes it
}

// StartReplica starts a replica (failover).
func StartReplica(ctx context.Context, replicaVMID, vmid int) error {
	for _, typ := range []string{"qemu", "lxc"} {
		p, err := configPath(typ, replicaVMID)
		if err != nil {
			continue
		}
		b, err := os.ReadFile(p) // #nosec G304 -- Proxmox VE configuration files and the agent's work files
		if err != nil {
			return err
		}
		if !strings.Contains(string(b), replicaMarker(vmid)) {
			return fmt.Errorf("guest %d is not the replica of guest %d", replicaVMID, vmid)
		}
		// Once started, its disks no longer hold the backup: the next update
		// compares every block.
		if err := os.WriteFile(p, []byte(stateToken.ReplaceAllString(string(b), "")), 0o640); err != nil { // #nosec G306 G703 -- p is configPath: the Proxmox folder and a guest number; 0640 as Proxmox writes it
			return err
		}
		_, err = command(ctx, guestTool(typ), "start", strconv.Itoa(replicaVMID))
		return err
	}
	return errors.New("replica " + strconv.Itoa(replicaVMID) + " does not exist on this node")
}

// stateToken records in a replica's description which backup it holds and
// when it was written. A started replica loses it (StartReplica), and so
// does one while an update writes its disks (Replicate), so the next update
// compares every block instead of only the changed ones.
var stateToken = regexp.MustCompile(` ?\[BackupZit state [0-9a-f]+ [0-9]+\]`)

func replicaState(snapshotID string, at time.Time) string {
	return fmt.Sprintf("[BackupZit state %s %d]", snapshotID, at.Unix())
}

// replicaDiverged tells why a replica's disks may no longer hold the backup
// prevID ("" when they do): the state token is missing (started through
// BackupZit, an update cut short, or written by an older version) or names
// another backup, or Proxmox VE started the guest since (for example in its
// web interface), or its task log cannot tell.
func replicaDiverged(ctx context.Context, conf, prevID, typ string, vmid int) string {
	var id string
	var at int64
	if m := stateToken.FindString(conf); m != "" {
		_, _ = fmt.Sscanf(strings.TrimSpace(m), "[BackupZit state %s %d]", &id, &at)
	}
	if id != prevID || at == 0 {
		return "the replica may have been started since its last update"
	}
	started, err := startedSince(ctx, typ, vmid, at)
	if err != nil {
		// Fail closed: a start in the web interface would go unseen, and
		// only the changes would be written over the diverged disks (L-N8).
		return "the Proxmox VE task log could not be read (" + err.Error() + "), so the replica may have been started since its last update"
	}
	if started != "" {
		return "the replica was started " + started + " since its last update"
	}
	return ""
}

// startedSince finds a start task of the guest in the Proxmox VE task log
// after the given time (Unix seconds): "" when there is none, an error when
// the log could not be read.
func startedSince(ctx context.Context, typ string, vmid int, since int64) (string, error) {
	filter := "qmstart"
	if typ == "lxc" {
		filter = "vzstart"
	}
	out, err := command(ctx, "pvesh", "get", "/nodes/"+LocalNode()+"/tasks", "--vmid", strconv.Itoa(vmid),
		"--typefilter", filter, "--since", strconv.FormatInt(since, 10), "--limit", "1", "--output-format", "json")
	if err != nil {
		return "", err
	}
	var tasks []struct {
		Start int64 `json:"starttime"`
	}
	if err := json.Unmarshal(out, &tasks); err != nil {
		return "", fmt.Errorf("unexpected task list: %w", err)
	}
	if len(tasks) == 0 {
		return "", nil
	}
	if tasks[0].Start == 0 {
		return "", errors.New("unexpected task list: a task without a start time")
	}
	if tasks[0].Start < since {
		return "", nil
	}
	return "on " + time.Unix(tasks[0].Start, 0).Local().Format("2006-01-02 15:04"), nil
}
