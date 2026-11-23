package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/leftover"
	"github.com/max-zit/backupzit/internal/vmlock"
)

// discoverAge is how old a BackupZit snapshot the ledger does not know must
// be before the janitor removes it (one of an older agent version, or of a
// lost ledger). Backups of this node hold the guest's lock while their
// snapshot exists; the age only covers a backup on another node still
// reading after the guest moved here.
const discoverAge = 24 * time.Hour

func snapshotItem(g Guest, name string) leftover.Item {
	return leftover.Item{Platform: leftover.Proxmox, Kind: leftover.KindSnapshot, VM: strconv.Itoa(g.VMID), VMName: g.Name, Name: name, Detail: g.Type}
}

func lockItem(g Guest) leftover.Item {
	return leftover.Item{Platform: leftover.Proxmox, Kind: leftover.KindLock, VM: strconv.Itoa(g.VMID), VMName: g.Name, Name: "backup", Detail: g.Type}
}

// guestConf reads a guest's configuration; ok is false when the guest has
// none (removed).
func guestConf(g Guest) (conf string, ok bool, err error) {
	p, err := configPath(g.Type, g.VMID)
	if err != nil {
		return "", false, nil
	}
	b, err := os.ReadFile(p) // #nosec G304 -- Proxmox VE configuration files
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return string(b), true, nil
}

// hasSnapshot tells whether the guest's configuration lists the snapshot.
func hasSnapshot(g Guest, name string) (bool, error) {
	conf, ok, err := guestConf(g)
	if err != nil || !ok {
		return false, err
	}
	for _, n := range snapshotNames(conf) {
		if n == name {
			return true, nil
		}
	}
	return false, nil
}

// configLock returns the guest's lock ("" when none).
func configLock(conf string) string {
	for _, line := range strings.Split(mainSection(conf), "\n") {
		if v, ok := strings.CutPrefix(line, "lock:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// removeGuestSnapshot removes a BackupZit snapshot and checks the guest's
// configuration that it is gone, with --force when the plain removal fails
// (forced). The ledger item goes once it is gone; otherwise it records the
// failure (S-1).
func removeGuestSnapshot(ctx context.Context, g Guest, name string, waits []time.Duration) (forced bool, err error) {
	it := snapshotItem(g, name)
	id := strconv.Itoa(g.VMID)
	err = leftover.Retry(ctx, waits, func(ctx context.Context) error {
		if ok, err := hasSnapshot(g, name); err != nil || !ok {
			return err
		}
		unmapSnapshot(ctx, name)
		c1, cancel := context.WithTimeout(ctx, 5*time.Minute)
		_, err1 := command(c1, guestTool(g.Type), "delsnapshot", id, name)
		cancel()
		if err1 == nil {
			return nil
		}
		// The retry gets its own time: the first try may have used up the
		// five minutes (a slow storage merging the snapshot).
		c2, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		if _, err2 := command(c2, guestTool(g.Type), "delsnapshot", id, name, "--force"); err2 != nil {
			return fmt.Errorf("%v; with --force: %v", lastLine(err1), lastLine(err2))
		}
		forced = true
		return nil
	}, func(context.Context) (bool, error) {
		ok, err := hasSnapshot(g, name)
		return !ok && err == nil, err
	})
	if err != nil {
		leftover.Failed(it, err)
		return forced, err
	}
	leftover.Done(it)
	return forced, nil
}

func lastLine(err error) string {
	s := strings.TrimSpace(err.Error())
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// unmapSnapshot unmaps Ceph RBD devices of a snapshot that a crashed backup
// left mapped (they would keep the snapshot from being removed).
func unmapSnapshot(ctx context.Context, name string) {
	if _, err := os.Stat("/sys/bus/rbd"); err != nil {
		return
	}
	out, err := command(ctx, "rbd", "showmapped", "--format", "json")
	if err != nil {
		return
	}
	var maps []struct {
		Snap   string `json:"snap"`
		Device string `json:"device"`
	}
	if json.Unmarshal(out, &maps) != nil {
		return
	}
	for _, m := range maps {
		if m.Snap == name && strings.HasPrefix(m.Device, "/dev/rbd") {
			_, _ = command(ctx, "rbd", "unmap", m.Device)
		}
	}
}

// activeTask tells whether Proxmox VE runs a task for the guest now (a
// vzdump backup, a migration, a snapshot): the janitor leaves it alone. A
// task list that cannot be read counts as busy.
func activeTask(ctx context.Context, vmid int) bool {
	out, err := command(ctx, "pvesh", "get", "/nodes/"+LocalNode()+"/tasks", "--source", "active", "--vmid", strconv.Itoa(vmid), "--output-format", "json")
	if err != nil {
		return true
	}
	var tasks []json.RawMessage
	if json.Unmarshal(out, &tasks) != nil {
		return true
	}
	return len(tasks) > 0
}

// localGuests lists the guests whose configuration is on this node.
func localGuests() []Guest {
	var out []Guest
	for _, t := range []struct{ dir, typ string }{{"qemu-server", "qemu"}, {"lxc", "lxc"}} {
		files, _ := filepath.Glob(filepath.Join(ConfigDir, "nodes", LocalNode(), t.dir, "*.conf"))
		for _, f := range files {
			id, err := strconv.Atoi(strings.TrimSuffix(filepath.Base(f), ".conf"))
			if err == nil {
				out = append(out, Guest{VMID: id, Type: t.typ})
			}
		}
	}
	return out
}

// snapshotTime reads the creation time from a BackupZit snapshot name
// (backupzit_<unix seconds>).
func snapshotTime(name string) (time.Time, bool) {
	n, err := strconv.ParseInt(strings.TrimPrefix(name, snapPrefix), 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}, false
	}
	return time.Unix(n, 0).UTC(), true
}

// CleanLeftovers removes what earlier backups on this node left: the
// snapshots and backup locks in the agent's ledger, and BackupZit snapshots
// older than discoverAge it does not know (S-2). Guests a run of this agent
// works on and guests with a running Proxmox VE task are skipped. It returns
// what was removed; the errors of the others are recorded in the ledger.
func CleanLeftovers(ctx context.Context, log func(string, ...any)) (removed []string, err error) {
	if log == nil {
		log = func(string, ...any) {}
	}
	known := map[string]bool{}
	var items []leftover.Item
	for _, it := range leftover.List() {
		if it.Platform == leftover.Proxmox {
			items = append(items, it)
			known[it.Key()] = true
		}
	}
	now := time.Now()
	for _, g := range localGuests() {
		// A guest a run works on may have a backup snapshot older than a
		// day (a very large guest): that one is not a leftover.
		unlock, free := vmlock.TryLock(vmLockKey(g.VMID))
		if !free {
			continue
		}
		unlock()
		conf, ok, err := guestConf(g)
		if err != nil || !ok {
			continue
		}
		for _, n := range snapshotNames(conf) {
			at, ok := snapshotTime(n)
			if !strings.HasPrefix(n, snapPrefix) || !ok || now.Sub(at) < discoverAge {
				continue
			}
			it := snapshotItem(g, n)
			it.Created = at
			if !known[it.Key()] {
				leftover.Add(context.Background(), it)
				items = append(items, it)
				known[it.Key()] = true
			}
		}
	}
	var errs []error
	for _, it := range items {
		vmid, err := strconv.Atoi(it.VM)
		if err != nil {
			leftover.Done(it)
			continue
		}
		g := Guest{VMID: vmid, Type: it.Detail, Name: it.VMName}
		if g.Type == "" {
			g.Type = "qemu"
		}
		unlock, free := vmlock.TryLock(vmLockKey(vmid))
		if !free {
			continue
		}
		done, rerr := cleanItem(ctx, g, it)
		unlock()
		switch {
		case rerr != nil:
			errs = append(errs, fmt.Errorf("guest %d %s: %w", vmid, it.Name, rerr))
		case done:
			removed = append(removed, fmt.Sprintf("%d/%s", vmid, it.Name))
			log("removed a leftover of an earlier backup", "vmid", vmid, "item", it.Kind+" "+it.Name)
		}
	}
	return removed, errors.Join(errs...)
}

// cleanItem removes one leftover; done is false when there was nothing to
// do now (a Proxmox VE task runs for the guest).
func cleanItem(ctx context.Context, g Guest, it leftover.Item) (done bool, err error) {
	conf, ok, err := guestConf(g)
	if err != nil {
		return false, err
	}
	if !ok {
		leftover.Done(it) // the guest is gone, and with it what it had
		return false, nil
	}
	if activeTask(ctx, g.VMID) {
		return false, nil
	}
	switch it.Kind {
	case leftover.KindSnapshot:
		if lk := configLock(conf); lk == "snapshot" || lk == "snapshot-delete" {
			// A crash during the creation or removal left Proxmox VE's own
			// lock of the snapshot task; no task runs, so it is stale.
			if _, err := command(ctx, guestTool(g.Type), "unlock", strconv.Itoa(g.VMID)); err != nil {
				leftover.Failed(it, err)
				return false, err
			}
		}
		_, err := removeGuestSnapshot(ctx, g, it.Name, nil)
		return err == nil, err
	case leftover.KindLock:
		// Only a lock the ledger says BackupZit set; vzdump's would show as
		// a running task above.
		if configLock(conf) == "backup" {
			if _, err := command(ctx, guestTool(g.Type), "unlock", strconv.Itoa(g.VMID)); err != nil {
				leftover.Failed(it, err)
				return false, err
			}
		}
		leftover.Done(it)
		return true, nil
	}
	leftover.Done(it)
	return false, nil
}
