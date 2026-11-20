package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/hyperv"
	"github.com/max-zit/backupzit/internal/leftover"
	"github.com/max-zit/backupzit/internal/pve"
)

// LeftoverPath is the service's ledger of what its backups created on the
// hypervisor (leftover), in its own folder.
func LeftoverPath() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(filepath.Dir(DefaultConfigPath()), "leftovers.json")
	}
	return "/var/lib/backupzit-agent/leftovers.json"
}

// janitorEvery is how often the agent works through its ledger (and right
// after it starts).
const janitorEvery = 15 * time.Minute

// janitor removes, in the background, what earlier backups on this
// Proxmox VE node or Hyper-V host left behind (S-2). VMs a run works on are
// skipped by their VM lock, so it also runs while other runs are active.
// VMware leftovers need the host's credentials: the agent removes them in
// its next VMware run or in a clean-up run the console queues.
func (a *Agent) janitor(ctx context.Context) {
	a.mu.Lock()
	due := !a.janitorRunning && time.Since(a.janitorAt) > janitorEvery
	if due {
		a.janitorRunning, a.janitorAt = true, time.Now()
	}
	a.mu.Unlock()
	if !due {
		return
	}
	go func() {
		defer func() {
			a.mu.Lock()
			a.janitorRunning = false
			a.mu.Unlock()
		}()
		cctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		defer cancel()
		a.cleanLocal(cctx)
	}()
}

// cleanLocal works through the ledger of this host's hypervisor.
func (a *Agent) cleanLocal(ctx context.Context) (removed []string, err error) {
	logf := func(msg string, kv ...any) { a.log.Info(msg, kv...) }
	switch {
	case pve.Available():
		removed, err = pve.CleanLeftovers(ctx, logf)
	case hyperv.Available():
		removed, err = hyperv.CleanLeftovers(ctx, logf)
	default:
		return nil, nil
	}
	if err != nil {
		a.log.Warn("leftovers of earlier backups could not all be removed", "removed", len(removed), "err", err)
	} else if len(removed) > 0 {
		a.log.Info("removed leftovers of earlier backups", "items", removed)
	}
	return removed, err
}

// leftoverReport returns the ledger items for the console when they changed
// since the last poll, or with the inventory; nil otherwise. An empty
// list ("[]") tells the console that nothing is left.
func (a *Agent) leftoverReport(withInventory bool) json.RawMessage {
	a.mu.Lock()
	active := make(map[int64]bool, len(a.active))
	for id := range a.active {
		active[id] = true
	}
	last := a.leftoversSent
	a.mu.Unlock()
	items := leftover.Report(func(id int64) bool { return active[id] })
	if items == nil {
		items = []leftover.Item{}
	}
	b, err := json.Marshal(items)
	if err != nil || (!withInventory && last != nil && bytes.Equal(b, last)) {
		return nil
	}
	return b
}

// leftoverCleanup runs a clean-up the console asked for (Remove now on the
// dashboard): on a VMware proxy for the host of the run, with the BackupZit
// snapshots the console found named in run.Cleanup; elsewhere this host's
// ledger.
func (a *Agent) leftoverCleanup(ctx context.Context, run api.Run) api.RunResult {
	var removed []string
	var err error
	if run.VMware != nil {
		c, cerr := vmwareConnect(ctx, run.VMware)
		if cerr != nil {
			return failed(cerr)
		}
		defer c.Logout()
		removed, err = c.CleanLeftovers(ctx, run.Cleanup, func(msg string, kv ...any) { a.log.Info(msg, append([]any{"run", run.ID}, kv...)...) })
	} else {
		if !pve.Available() && !hyperv.Available() {
			return failed(fmt.Errorf("this machine is neither a Proxmox VE node nor a Hyper-V host"))
		}
		removed, err = a.cleanLocal(ctx)
	}
	a.mu.Lock()
	a.leftoversSent = nil // the console sees the new list at the next poll
	a.mu.Unlock()
	left := 0
	for _, it := range leftover.List() {
		if (run.VMware == nil && it.Platform != leftover.VMware) || (run.VMware != nil && it.Platform == leftover.VMware && strings.EqualFold(it.Host, run.VMware.Address)) {
			left++
		}
	}
	msg := fmt.Sprintf("Removed %s", count(len(removed), "leftover", "leftovers"))
	if len(removed) == 0 {
		msg = "Nothing was removed"
	}
	if left > 0 {
		msg += fmt.Sprintf("; %d %s (in use by a run, or the removal failed: see the errors)", left, map[bool]string{true: "is left", false: "are left"}[left == 1])
	}
	res := api.RunResult{Status: api.StatusSuccess, Message: msg}
	if err != nil {
		res.Status = api.StatusWarning
		res.Errors = strings.Split(err.Error(), "\n")
	} else if left > 0 {
		res.Status = api.StatusWarning
	}
	return res
}
