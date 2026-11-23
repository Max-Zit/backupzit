package hyperv

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/leftover"
)

func checkpointItem(vm vmInfo, name string) leftover.Item {
	return leftover.Item{Platform: leftover.HyperV, Kind: leftover.KindCheckpoint, VM: vm.ID, VMName: vm.Name, Name: name, Detail: doneMarker(vm, name)}
}

// errMoved: the VM is no longer on this host (live migration).
var errMoved = errors.New("the VM moved to another node")

// checkpointExists tells whether the VM has a checkpoint of that name.
func checkpointExists(ctx context.Context, id, name string) (bool, error) {
	out, err := ps(ctx, `$vm = Get-VM -Id `+psq(id)+` -ErrorAction SilentlyContinue; if (-not $vm) { 'moved' } else { @(Get-VMSnapshot -VM $vm -Name `+psq(name)+` -ErrorAction SilentlyContinue).Count }`)
	if err != nil {
		return false, err
	}
	switch s := strings.TrimSpace(string(out)); s {
	case "moved":
		return false, errMoved
	case "0":
		return false, nil
	default:
		n, err := strconv.Atoi(s)
		if err != nil {
			return false, fmt.Errorf("unexpected answer %q", s)
		}
		return n > 0, nil
	}
}

// removeCheckpointVerified removes the checkpoint called name and checks
// that it is gone, up to len(waits)+1 times; the ledger item goes with it
// or records the failure (S-1). errMoved is returned unchanged.
func removeCheckpointVerified(ctx context.Context, vm vmInfo, name string, waits []time.Duration) error {
	it := checkpointItem(vm, name)
	err := leftover.Retry(ctx, waits, func(ctx context.Context) error {
		ok, err := checkpointExists(ctx, vm.ID, name)
		if err != nil || !ok {
			return err
		}
		_, err = ps(ctx, `Get-VMSnapshot -VM (Get-VM -Id `+psq(vm.ID)+`) -Name `+psq(name)+` -ErrorAction Stop | Remove-VMSnapshot -ErrorAction Stop`)
		if err != nil {
			return errors.New(lastLine(err.Error()))
		}
		return nil
	}, func(ctx context.Context) (bool, error) {
		ok, err := checkpointExists(ctx, vm.ID, name)
		if errors.Is(err, errMoved) {
			return false, err
		}
		return !ok && err == nil, err
	})
	if errors.Is(err, errMoved) {
		return errMoved
	}
	if err != nil {
		leftover.Failed(it, err)
		return err
	}
	leftover.Done(it)
	return nil
}

// pendingAge: a checkpoint whose creation was cancelled is looked for under
// its default name for this long; then the record goes.
const pendingAge = time.Hour

// removeDefaultNamed removes the checkpoint Hyper-V made under its default
// name ("VM - (date time)") for a creation that was cancelled before the
// checkpoint got its BackupZit name: a recovery checkpoint (only backup
// software makes those) created in the time of that creation, and only
// when there is exactly one. It reports whether one was removed.
func removeDefaultNamed(ctx context.Context, it leftover.Item) (bool, error) {
	from := it.Created.Add(-time.Minute).Unix()
	to := it.Created.Add(checkpointGrace + 15*time.Minute).Unix()
	sel := `$vm = Get-VM -Id ` + psq(it.VM) + ` -ErrorAction Stop; $from = [DateTimeOffset]::FromUnixTimeSeconds(` + strconv.FormatInt(from, 10) + `).LocalDateTime; $to = [DateTimeOffset]::FromUnixTimeSeconds(` + strconv.FormatInt(to, 10) + `).LocalDateTime
$c = @(Get-VMSnapshot -VM $vm | Where-Object { "$($_.SnapshotType)" -eq 'Recovery' -and $_.Name -notlike '` + snapPrefix + `*' -and $_.CreationTime -ge $from -and $_.CreationTime -le $to })
`
	out, err := ps(ctx, sel+`if ($c.Count -eq 1) { $c[0] | Remove-VMSnapshot -ErrorAction Stop; 'removed' } else { "$($c.Count)" }`)
	if err != nil {
		return false, errors.New(lastLine(err.Error()))
	}
	return strings.TrimSpace(string(out)) == "removed", nil
}

// CleanLeftovers removes the checkpoints in the agent's ledger that earlier
// backups on this host could not remove (agent crash, power loss, a
// removal that failed, a creation cancelled before the checkpoint was
// named), S-2. VMs a backup on this host uses are skipped. A VM that moved
// to another node gets its completion marker, so the agent there removes
// the checkpoint (CleanupCheckpoints). It returns what was removed; the
// errors of the others are recorded in the ledger.
func CleanLeftovers(ctx context.Context, log func(string, ...any)) (removed []string, err error) {
	if log == nil {
		log = func(string, ...any) {}
	}
	var errs []error
	for _, it := range leftover.List() {
		if it.Platform != leftover.HyperV {
			continue
		}
		if !guidRe.MatchString(it.VM) {
			leftover.Done(it)
			continue
		}
		unlock, err := lockVM(ctx, it.VM, false)
		if errors.Is(err, errVMBusy) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		vm := vmInfo{ID: it.VM, Name: it.VMName}
		done, rerr := cleanCheckpoint(ctx, vm, it)
		unlock()
		switch {
		case rerr != nil:
			errs = append(errs, fmt.Errorf("%s of VM %s: %w", it.Name, it.VMName, rerr))
		case done != "":
			removed = append(removed, it.VMName+"/"+done)
			log("removed a leftover checkpoint of an earlier backup", "vm", it.VMName, "checkpoint", done)
		}
	}
	return removed, errors.Join(errs...)
}

// cleanCheckpoint handles one ledger item; done names the checkpoint
// removed ("" when none).
func cleanCheckpoint(ctx context.Context, vm vmInfo, it leftover.Item) (done string, err error) {
	ok, err := checkpointExists(ctx, vm.ID, it.Name)
	switch {
	case errors.Is(err, errMoved):
		// No backup on this host reads it any more: the marker lets the
		// node that runs the VM now remove it.
		if it.Detail != "" {
			if werr := os.WriteFile(it.Detail, nil, 0o600); werr != nil {
				leftover.Failed(it, fmt.Errorf("the VM moved to another node and the marker for its agent could not be written: %w", werr))
				return "", werr
			}
		}
		leftover.Done(it)
		return "", nil
	case err != nil:
		leftover.Failed(it, err)
		return "", err
	case ok:
		if err := removeCheckpointVerified(ctx, vm, it.Name, nil); err != nil {
			return "", err
		}
		return it.Name, nil
	case it.Pending:
		n, err := removeDefaultNamed(ctx, it)
		if err != nil {
			leftover.Failed(it, err)
			return "", err
		}
		if n {
			leftover.Done(it)
			return "the checkpoint with the default name made for " + it.Name, nil
		}
		if time.Since(it.Created) < pendingAge {
			return "", nil // Hyper-V may still be making it
		}
	}
	leftover.Done(it)
	return "", nil
}
