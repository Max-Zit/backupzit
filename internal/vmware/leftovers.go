package vmware

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/leftover"
	"github.com/max-zit/backupzit/internal/vmlock"
)

// copyItem is the ledger record of a temporary disk copy of vm.
func (c *Client) copyItem(vm VM, p dsPath) leftover.Item {
	return leftover.Item{Platform: leftover.VMware, Kind: leftover.KindDiskCopy, Host: c.conn.Host, VM: vm.UUID, VMName: vm.Name, Name: p.String()}
}

// fileExists tells whether a datastore file exists; unlike exists it
// returns an error when it cannot tell.
func (c *Client) fileExists(ctx context.Context, p dsPath) (bool, error) {
	if c.useAPI {
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.fileURL(p), nil)
		if err != nil {
			return false, err
		}
		status := 0
		if err := c.vim.Do(ctx, req, func(resp *http.Response) error {
			status = resp.StatusCode
			return nil
		}); err != nil && status == 0 {
			return false, err
		}
		switch status {
		case http.StatusOK:
			return true, nil
		case http.StatusNotFound:
			return false, nil
		}
		return false, fmt.Errorf("datastore browser answered %d", status)
	}
	out, err := c.run(ctx, "if test -e "+sq(p.local())+"; then echo yes; else echo no; fi")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "yes", nil
}

// deleteCopy deletes a temporary disk copy and checks that it is gone; the
// ledger item goes with it, or records the failure.
func (c *Client) deleteCopy(ctx context.Context, vm VM, p dsPath) error {
	it := c.copyItem(vm, p)
	err := leftover.Retry(ctx, nil, func(ctx context.Context) error {
		if ok, err := c.fileExists(ctx, p); err != nil || !ok {
			return err
		}
		return c.deleteDisk(ctx, p)
	}, func(ctx context.Context) (bool, error) {
		ok, err := c.fileExists(ctx, p)
		return !ok && err == nil, err
	})
	if err != nil {
		leftover.Failed(it, err)
		return err
	}
	leftover.Done(it)
	return nil
}

// CleanLeftovers removes what earlier backups of this agent left on this
// host or vCenter (the agent's ledger), and the BackupZit snapshots named in
// extra (asked for in the console). A VM a run of this agent works on is
// skipped; so is a snapshot without the BackupZit prefix. It returns what was
// removed; the errors of the others are recorded in the ledger.
func (c *Client) CleanLeftovers(ctx context.Context, extra []leftover.Item, log func(string, ...any)) (removed []string, err error) {
	if log == nil {
		log = func(string, ...any) {}
	}
	var items []leftover.Item
	for _, it := range leftover.List() {
		if it.Platform == leftover.VMware && strings.EqualFold(it.Host, c.conn.Host) {
			items = append(items, it)
		}
	}
	for _, it := range extra {
		if it.Kind != leftover.KindSnapshot || !strings.HasPrefix(it.Name, snapPrefix) {
			continue
		}
		it.Platform, it.Host = leftover.VMware, c.conn.Host
		leftover.Add(ctx, it)
		items = append(items, it)
	}
	if len(items) == 0 {
		return nil, nil
	}
	vms, err := c.VMs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list VMs: %w", err)
	}
	byUUID := map[string]VM{}
	for _, v := range vms {
		byUUID[strings.ToLower(v.UUID)] = v
	}
	var errs []error
	seen := map[string]bool{}
	for _, it := range items {
		if seen[it.Key()] {
			continue
		}
		seen[it.Key()] = true
		v, ok := byUUID[strings.ToLower(it.VM)]
		if !ok && it.Kind == leftover.KindDiskCopy {
			v, ok = VM{UUID: it.VM, Name: it.VMName}, true // the copy outlives its VM
		}
		if !ok {
			// The VM is gone (deleted, or moved to a host of another
			// connection): with it its snapshots and folder.
			log("leftover of a VM that no longer exists dropped", "vm", it.VMName, "item", it.Name)
			leftover.Done(it)
			continue
		}
		unlock, free := vmlock.TryLock(c.vmLockKey(v.UUID))
		if !free {
			continue // a backup or restore of this agent works on it
		}
		var rerr error
		switch it.Kind {
		case leftover.KindSnapshot:
			var note string
			note, rerr = c.removeSnapshotVerified(ctx, v, it.Name, nil)
			if note != "" {
				log(note, "vm", v.Name)
			}
		case leftover.KindDiskCopy:
			var p dsPath
			if p, rerr = parseDSPath(it.Name); rerr == nil {
				if !strings.HasPrefix(p.Path, tmpDir+"/") {
					rerr = errors.New("not a BackupZit disk copy")
				} else {
					rerr = c.deleteCopy(ctx, v, p)
					c.removeDirIfEmpty(ctx, p.DS, tmpDir)
				}
			}
		default:
			leftover.Done(it) // nothing this version knows how to remove
		}
		unlock()
		if rerr != nil {
			errs = append(errs, fmt.Errorf("%s of VM %s: %w", it.Name, v.Name, rerr))
			continue
		}
		removed = append(removed, v.Name+"/"+it.Name)
		log("removed a leftover of an earlier backup", "vm", v.Name, "item", it.Name)
	}
	return removed, errors.Join(errs...)
}

// cleanAfterBackup works through the ledger once the run's own VMs are done;
// errors stay in the ledger (and so on the dashboard).
func (c *Client) cleanAfterBackup(ctx context.Context, log func(string, ...any)) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if removed, err := c.CleanLeftovers(cctx, nil, log); err != nil {
		log("leftovers of earlier backups could not all be removed", "removed", len(removed), "err", err)
	}
}
