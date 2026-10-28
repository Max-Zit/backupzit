package pve

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/diskimg"
	"github.com/max-zit/backupzit/internal/nbd"
	"github.com/max-zit/backupzit/internal/repo"
)

// Changed block tracking for running VMs. Just before the snapshot of a
// backup, every disk of the VM gets a QEMU dirty bitmap, which records all
// writes from then on; its name is stored with the backup. The next backup
// reads only the areas of that bitmap (through a read-only NBD export of
// the disk that QEMU serves on a local socket) and takes everything else
// from the previous backup. The bitmaps live in the QEMU process: after a
// restart of the VM, or when anything does not fit (other disk size, disk
// moved, bitmap gone), the disk is read completely as before.

const bitmapPrefix = "backupzit-"

// bitmapMaxAge: bitmaps of jobs that stopped running are removed.
const bitmapMaxAge = 60 * 24 * time.Hour

// nbdSocketDir holds the sockets of the NBD exports; dirtyAreas reads a
// bitmap (tests replace both).
var (
	nbdSocketDir = "/run/backupzit"
	dirtyAreas   = nbd.DirtyAreas
)

// qmpScript runs QMP commands through Proxmox VE's own monitor client,
// which serializes access to the VM's monitor socket.
const qmpScript = `use strict; use warnings; use JSON;
use PVE::QemuServer::Monitor qw(mon_cmd);
my ($vmid, $in) = @ARGV;
my @out;
for my $c (@{decode_json($in)}) {
  my $res = eval { mon_cmd($vmid, $c->{execute}, %{$c->{arguments} // {}}) };
  push @out, $@ ? { error => "$@" } : { return => $res };
}
print encode_json(\@out);
`

type qmpCmd struct {
	Execute   string         `json:"execute"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

type qmpResult struct {
	Return json.RawMessage `json:"return"`
	Error  string          `json:"error"`
}

// qmp runs commands on a VM's monitor; each has its own result.
var qmp = func(ctx context.Context, vmid int, cmds ...qmpCmd) ([]qmpResult, error) {
	in, _ := json.Marshal(cmds)
	out, err := command(ctx, "perl", "-e", qmpScript, strconv.Itoa(vmid), string(in))
	if err != nil {
		return nil, err
	}
	var res []qmpResult
	if err := json.Unmarshal(out, &res); err != nil || len(res) != len(cmds) {
		return nil, fmt.Errorf("monitor answer: %.200s", out)
	}
	return res, nil
}

func qmpOne(ctx context.Context, vmid int, cmd string, args map[string]any) (json.RawMessage, error) {
	res, err := qmp(ctx, vmid, qmpCmd{cmd, args})
	if err != nil {
		return nil, err
	}
	if res[0].Error != "" {
		return nil, fmt.Errorf("%s: %s", cmd, strings.TrimSpace(res[0].Error))
	}
	return res[0].Return, nil
}

// blockDevice is a disk of a running VM as query-block reports it.
type blockDevice struct {
	Device   string `json:"device"`
	QDev     string `json:"qdev"`
	Inserted *struct {
		NodeName string `json:"node-name"`
		Bitmaps  []struct {
			Name string `json:"name"`
		} `json:"dirty-bitmaps"`
	} `json:"inserted"`
}

// trackedDisk is where a disk's bitmaps are.
type trackedDisk struct {
	bitmapNode string // device or node name the bitmaps are on
	exportNode string // node name exported over NBD
	bitmaps    []string
}

// tracker manages the bitmaps of one VM during a backup.
type tracker struct {
	vmid    int
	name    string // the bitmap created for this backup
	disks   map[string]*trackedDisk
	sock    string
	started bool              // this backup started QEMU's NBD server
	used    map[string]string // disk key → bitmap of the previous backup it read
	log     func(string, ...any)
}

var deviceKey = regexp.MustCompile(`(ide|sata|scsi|virtio)\d+`)

// diskKeyOf maps a query-block entry to its disk key (scsi0, virtio1, ...).
func diskKeyOf(d blockDevice) string {
	for _, s := range []string{d.Device, d.QDev} {
		s = strings.TrimPrefix(s, "drive-")
		for _, part := range strings.Split(s, "/") {
			if deviceKey.MatchString(part) && deviceKey.FindString(part) == part {
				return part
			}
		}
	}
	if d.Inserted != nil {
		if k := strings.TrimPrefix(d.Inserted.NodeName, "drive-"); deviceKey.FindString(k) == k {
			return k
		}
	}
	return ""
}

// startTracking adds a bitmap to every disk of the running VM, just
// before its snapshot is taken. keys are the disk keys backed up.
func startTracking(ctx context.Context, vmid int, keys []string, log func(string, ...any)) (*tracker, error) {
	raw, err := qmpOne(ctx, vmid, "query-block", nil)
	if err != nil {
		return nil, err
	}
	var devs []blockDevice
	if err := json.Unmarshal(raw, &devs); err != nil {
		return nil, fmt.Errorf("query-block: %w", err)
	}
	want := map[string]bool{}
	for _, k := range keys {
		want[k] = true
	}
	var suffix [4]byte
	rand.Read(suffix[:])
	t := &tracker{vmid: vmid, disks: map[string]*trackedDisk{}, used: map[string]string{}, log: log,
		name: fmt.Sprintf("%s%d-%s", bitmapPrefix, time.Now().Unix(), hex.EncodeToString(suffix[:]))}
	var add []map[string]any
	var stale []qmpCmd
	for _, d := range devs {
		k := diskKeyOf(d)
		if !want[k] || d.Inserted == nil || d.Inserted.NodeName == "" || t.disks[k] != nil {
			continue
		}
		// Bitmaps go on a named device or node: QEMU can migrate those
		// with the VM, bitmaps on nodes with generated names would stop
		// a migration.
		node := d.Device
		if node == "" && !strings.HasPrefix(d.Inserted.NodeName, "#") {
			node = d.Inserted.NodeName
		}
		if node == "" {
			continue
		}
		td := &trackedDisk{bitmapNode: node, exportNode: d.Inserted.NodeName}
		for _, b := range d.Inserted.Bitmaps {
			if !strings.HasPrefix(b.Name, bitmapPrefix) {
				continue
			}
			if bitmapTime(b.Name).Before(time.Now().Add(-bitmapMaxAge)) {
				stale = append(stale, qmpCmd{"block-dirty-bitmap-remove", map[string]any{"node": node, "name": b.Name}})
				continue
			}
			td.bitmaps = append(td.bitmaps, b.Name)
		}
		t.disks[k] = td
		add = append(add, map[string]any{"type": "block-dirty-bitmap-add",
			"data": map[string]any{"node": node, "name": t.name, "persistent": false}})
	}
	if len(stale) > 0 {
		qmp(ctx, vmid, stale...)
	}
	if len(add) == 0 {
		return nil, errors.New("no disks with changed block tracking")
	}
	// One transaction: all disks start tracking at the same moment.
	if _, err := qmpOne(ctx, vmid, "transaction", map[string]any{"actions": add}); err != nil {
		return nil, err
	}
	return t, nil
}

// bitmapTime is when a BackupZit bitmap was created (zero if unknown).
func bitmapTime(name string) time.Time {
	s, _, _ := strings.Cut(strings.TrimPrefix(name, bitmapPrefix), "-")
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

// tracking reports whether disk key got this backup's bitmap.
func (t *tracker) tracking(key string) bool { return t != nil && t.disks[key] != nil }

// changes returns the areas of disk key written since the bitmap prev was
// created, and the disk size as QEMU sees it.
func (t *tracker) changes(ctx context.Context, key, prev string) ([]diskimg.Area, uint64, error) {
	td := t.disks[key]
	if td == nil {
		return nil, 0, errors.New("disk not tracked")
	}
	found := false
	for _, b := range td.bitmaps {
		found = found || b == prev
	}
	if !found {
		return nil, 0, errors.New("the bitmap of the previous backup is gone (the VM was restarted or moved)")
	}
	if !t.started {
		dir := nbdSocketDir
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, 0, err
		}
		t.sock = fmt.Sprintf("%s/nbd-%d.sock", dir, t.vmid)
		os.Remove(t.sock)
		if _, err := qmpOne(ctx, t.vmid, "nbd-server-start", map[string]any{
			"addr": map[string]any{"type": "unix", "data": map[string]any{"path": t.sock}}}); err != nil {
			return nil, 0, err // e.g. a migration uses QEMU's NBD server now
		}
		t.started = true
	}
	// QEMU exports only bitmaps that no longer record: a stopped copy of
	// the previous bitmap is read, the bitmap itself stays as it is (still
	// the base if this backup fails).
	id := bitmapPrefix + key
	read := fmt.Sprintf("bzread-%s-%d", key, time.Now().UnixNano())
	if _, err := qmpOne(ctx, t.vmid, "transaction", map[string]any{"actions": []map[string]any{
		{"type": "block-dirty-bitmap-add", "data": map[string]any{"node": td.bitmapNode, "name": read, "persistent": false, "disabled": true}},
		{"type": "block-dirty-bitmap-merge", "data": map[string]any{"node": td.bitmapNode, "target": read, "bitmaps": []string{prev}}},
	}}); err != nil {
		return nil, 0, err
	}
	defer qmpOne(context.WithoutCancel(ctx), t.vmid, "block-dirty-bitmap-remove", map[string]any{"node": td.bitmapNode, "name": read})
	if _, err := qmpOne(ctx, t.vmid, "block-export-add", map[string]any{"type": "nbd", "id": id, "node-name": td.exportNode,
		"name": id, "writable": false, "bitmaps": []string{read}}); err != nil {
		return nil, 0, err
	}
	defer qmpOne(context.WithoutCancel(ctx), t.vmid, "block-export-del", map[string]any{"id": id, "mode": "hard"})
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	areas, size, err := dirtyAreas(cctx, "unix:"+t.sock, id, read)
	if err != nil {
		return nil, 0, err
	}
	t.used[key] = prev
	out := make([]diskimg.Area, len(areas))
	for i, a := range areas {
		out[i] = diskimg.Area{Offset: a.Offset, Length: a.Length}
	}
	return out, size, nil
}

// finish stops the NBD server and removes the bitmaps no longer needed:
// after a saved backup the ones of the previous backup it read, otherwise
// the one created for this backup (the previous backup stays the base).
func (t *tracker) finish(ok bool) {
	if t == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	t.stopServer()
	var cmds []qmpCmd
	for k, td := range t.disks {
		switch {
		case !ok:
			cmds = append(cmds, qmpCmd{"block-dirty-bitmap-remove", map[string]any{"node": td.bitmapNode, "name": t.name}})
		case t.used[k] != "":
			cmds = append(cmds, qmpCmd{"block-dirty-bitmap-remove", map[string]any{"node": td.bitmapNode, "name": t.used[k]}})
		}
	}
	if len(cmds) == 0 {
		return
	}
	res, err := qmp(ctx, t.vmid, cmds...)
	if err != nil {
		t.log("changed block tracking: cleanup failed", "vmid", t.vmid, "err", err)
		return
	}
	for i, r := range res {
		if r.Error != "" {
			t.log("changed block tracking: cleanup", "vmid", t.vmid, "command", cmds[i].Execute, "err", strings.TrimSpace(r.Error))
		}
	}
}

// previousDisks finds, per "<vmid>/<disk key>", the newest backup of each
// Proxmox VM disk in r that recorded a bitmap.
func previousDisks(ctx context.Context, r *repo.Repository) map[string]diskimg.Previous {
	out := map[string]diskimg.Previous{}
	snaps, err := r.ListSnapshots(ctx)
	if err != nil {
		return out
	}
	for i := len(snaps) - 1; i >= 0; i-- { // newest first
		sn := snaps[i]
		for _, g := range sn.Guests {
			if g.Platform != "proxmox" || g.Type != "qemu" {
				continue
			}
			for _, d := range g.Disks {
				k := fmt.Sprintf("%d/%s", g.VMID, d.Key)
				if _, seen := out[k]; seen || d.ChangeID == "" || d.Image < 0 || d.Image >= len(sn.Images) {
					continue
				}
				img := sn.Images[d.Image]
				out[k] = diskimg.Previous{Image: &img, ChangeID: d.ChangeID}
			}
		}
	}
	return out
}

// stopServer ends the NBD export of the VM's disks after they were read.
func (t *tracker) stopServer() {
	if t == nil || !t.started {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	qmpOne(ctx, t.vmid, "nbd-server-stop", nil)
	os.Remove(t.sock)
	t.started = false
}

// verify checks after the snapshot that every tracked disk still has this
// backup's bitmap on the node the VM writes to. A snapshot that changes
// QEMU's block graph (a new overlay, as with snapshots as volume chains)
// would leave the bitmap behind on a node that gets no writes any more.
func (t *tracker) verify(ctx context.Context) {
	if t == nil {
		return
	}
	raw, err := qmpOne(ctx, t.vmid, "query-block", nil)
	var devs []blockDevice
	if err == nil {
		err = json.Unmarshal(raw, &devs)
	}
	has := map[string]bool{}
	for _, d := range devs {
		if d.Inserted == nil {
			continue
		}
		for _, b := range d.Inserted.Bitmaps {
			if b.Name == t.name {
				has[diskKeyOf(d)] = true
			}
		}
	}
	for k, td := range t.disks {
		if err != nil || !has[k] {
			t.log("changed block tracking stopped for a disk: the snapshot changed its block nodes", "vmid", t.vmid, "disk", k, "err", err)
			qmpOne(ctx, t.vmid, "block-dirty-bitmap-remove", map[string]any{"node": td.bitmapNode, "name": t.name})
			delete(t.disks, k)
		}
	}
}

// cbtStorage reports whether snapshots of a volume leave QEMU's block
// graph alone (block device snapshots, qcow2 internal snapshots), which
// changed block tracking needs.
func cbtStorage(st storage, vol string) bool {
	switch st.Type {
	case "lvmthin", "rbd", "zfspool":
		return true
	}
	return volumeFormat(vol) == "qcow2" && st.Props["snapshot-as-volume-chain"] != "1"
}
