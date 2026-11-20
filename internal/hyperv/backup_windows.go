package hyperv

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/diskimg"
	"github.com/max-zit/backupzit/internal/leftover"
	"github.com/max-zit/backupzit/internal/repo"
)

// BackupOptions select the VMs to back up.
type BackupOptions struct {
	// VMIDs lists VMs by their BackupZit number (VMIDFor); nil means all.
	VMIDs   []int
	Exclude []int
	// Cluster: this host is a node of a failover cluster and the job is the
	// cluster's. "All" means its clustered VMs, and selected VMs owned by
	// another node are left to that node (ErrNoneHere when none is here).
	Cluster  bool
	Hostname string
	Version  string
	Tags     []string
	// RequireAppConsistent: no crash-consistent fallback (see ErrNotAppConsistent).
	RequireAppConsistent bool
	// ExcludeDisks are disk files left out (see DiskExcluded).
	ExcludeDisks []string
	// CrashConsistentOK lists VMs (names or patterns, see
	// CrashConsistentExpected) whose crash-consistent checkpoint is expected:
	// no warning for them, the guest still reports it as crash-consistent.
	CrashConsistentOK []string
	Progress          func(done, total uint64)
	Log               func(msg string, args ...any)
}

// Backup checkpoints the selected VMs and stores their disks and settings
// in one snapshot. VMs that fail are listed in the snapshot's errors.
func Backup(ctx context.Context, r *repo.Repository, opts BackupOptions) (*repo.Snapshot, error) {
	if opts.Log == nil {
		opts.Log = func(string, ...any) {}
	}
	vms, err := listVMs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list VMs: %w", err)
	}
	if opts.Hostname == "" {
		opts.Hostname, _ = os.Hostname()
	}
	var stats repo.SnapshotStats
	addErr := func(item string, err error) { stats.Errors = append(stats.Errors, fmt.Sprintf("%s: %v", item, err)) }
	byID := map[int]vmInfo{}
	for _, vm := range vms {
		byID[VMIDFor(vm.ID)] = vm
	}
	here := make([]hostVM, 0, len(vms))
	for _, vm := range vms {
		here = append(here, hostVM{ID: VMIDFor(vm.ID), Clustered: vm.Clustered})
	}
	ids, missing := pick(here, opts.VMIDs, opts.Exclude, opts.Cluster)
	for _, id := range missing {
		addErr(fmt.Sprintf("VM %d", id), errors.New("does not exist on this host (any more)"))
	}
	var sel []vmInfo
	for _, id := range ids {
		sel = append(sel, byID[id])
	}
	if len(sel) == 0 && len(stats.Errors) == 0 {
		if opts.Cluster {
			return nil, ErrNoneHere
		}
		return nil, errors.New("no virtual machines on this Hyper-V host")
	}
	prev, err := diskimg.PreviousDisks(ctx, r, "hyperv")
	if err != nil {
		return nil, fmt.Errorf("read earlier backups: %w", err)
	}
	var total, done uint64
	for _, vm := range sel {
		total += vm.DiskSize
	}
	progress := func(n uint64) {
		done += n
		if opts.Progress != nil {
			opts.Progress(done, max(total, done))
		}
	}
	start := time.Now()
	before := r.Stats()
	sn := &repo.Snapshot{Time: start.UTC(), Hostname: opts.Hostname, Tags: append([]string{"hyperv"}, opts.Tags...), ProgramVersion: opts.Version}
	notApp := 0
	for _, vm := range sel {
		opts.Log("backing up VM", "name", vm.Name, "id", vm.ID)
		g, warns, err := backupVM(ctx, r, vm, sn, prev, progress, opts.RequireAppConsistent, CrashConsistentExpected(vm.Name, opts.CrashConsistentOK), opts.ExcludeDisks, opts.Log)
		for _, w := range warns {
			addErr(vm.Name, errors.New(w))
		}
		if err != nil {
			if ctx.Err() != nil {
				for _, w := range warns { // the run ends without them: keep them in the agent log
					opts.Log("warning of the cancelled backup", "vm", vm.Name, "warning", w)
				}
				return nil, ctx.Err()
			}
			if errors.Is(err, ErrNotAppConsistent) {
				notApp++
			}
			addErr(vm.Name, err)
			stats.Failed = append(stats.Failed, vm.Name)
			continue
		}
		sn.Guests = append(sn.Guests, *g)
		sn.Paths = append(sn.Paths, "hyperv/"+vm.ID)
		stats.Files++
	}
	if len(sn.Guests) == 0 {
		err := fmt.Errorf("no VM could be backed up: %s", strings.Join(stats.Errors, "; "))
		if notApp > 0 {
			err = notAppErr{err.Error()}
		}
		return nil, err
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
	if notApp > 0 {
		return sn, fmt.Errorf("%w for %d of %d VMs; they were not backed up", ErrNotAppConsistent, notApp, len(sel))
	}
	return sn, nil
}

type checkpointInfo struct {
	Type string `json:"type"`
	// State is "Off" for production checkpoints (no saved memory) of a
	// running VM; Hyper-V labels both kinds "Standard".
	State string   `json:"state"`
	Disks []vmDisk `json:"disks"`
}

// recoveryScript creates a recovery checkpoint (the checkpoint type backup
// applications use; it switches on Resilient Change Tracking of the disks)
// with the given consistency level (1 application, 2 crash), names it and
// prints its disks as JSON.
const recoveryScript = `$ns = 'root\virtualization\v2'
$vm = Get-CimInstance -Namespace $ns -ClassName Msvm_ComputerSystem -Filter ("Name='" + $id + "'")
$svc = Get-CimInstance -Namespace $ns -ClassName Msvm_VirtualSystemSnapshotService
$cls = Get-CimClass -Namespace $ns -ClassName Msvm_VirtualSystemSnapshotSettingData
$sd = New-CimInstance -CimClass $cls -ClientOnly -Property @{ConsistencyLevel = [byte]$level; IgnoreNonSnapshottableDisks = $true}
$ser = [Microsoft.Management.Infrastructure.Serialization.CimSerializer]::Create()
$text = [System.Text.Encoding]::Unicode.GetString($ser.Serialize($sd, [Microsoft.Management.Infrastructure.Serialization.InstanceSerializationOptions]::None))
$v = Get-VM -Id $id
$before = @(Get-VMSnapshot -VM $v | Select-Object -ExpandProperty Id)
$r = Invoke-CimMethod -InputObject $svc -MethodName CreateSnapshot -Arguments @{AffectedSystem = $vm; SnapshotSettings = $text; SnapshotType = [uint16]32768}
if ($r.ReturnValue -eq 4096) {
    $job = Get-CimInstance -InputObject $r.Job
    while ($job.JobState -lt 7) { Start-Sleep -Milliseconds 500; $job = Get-CimInstance -InputObject $r.Job }
    if ($job.JobState -ne 7) { throw ("recovery checkpoint: " + $job.ErrorDescription) }
} elseif ($r.ReturnValue -ne 0) { throw ("recovery checkpoint: error " + $r.ReturnValue) }
# The new checkpoint appears in the list shortly after the job.
$s = $null
for ($i = 0; $i -lt 30 -and -not $s; $i++) {
    $s = Get-VMSnapshot -VM $v | Where-Object { $before -notcontains $_.Id } | Select-Object -First 1
    if (-not $s) { Start-Sleep -Milliseconds 500 }
}
if (-not $s) { throw "recovery checkpoint not found" }
try { Rename-VMSnapshot -VMSnapshot $s -NewName $name } catch { Remove-VMSnapshot -VMSnapshot $s; throw }
[pscustomobject]@{ type = "Recovery"; state = "$($s.State)"; disks = @(Get-VMHardDiskDrive -VMSnapshot $s | ForEach-Object { [pscustomobject]@{ type = "$($_.ControllerType)"; num = $_.ControllerNumber; loc = $_.ControllerLocation; path = $_.Path } }) } | ConvertTo-Json -Depth 4 -Compress`

// checkpointGrace bounds how long a cancelled backup still waits for a
// checkpoint being created (and named), see takeCheckpoint. It is shorter
// than the console's wait for a cancelled run to stop. Without a cancel
// creating has no time limit, as before: a slow application-consistent
// checkpoint must not be cut off and left behind.
var checkpointGrace = 5 * time.Minute

// takeCheckpoint creates the backup's checkpoint: a recovery checkpoint
// (application-consistent, else crash-consistent) where Hyper-V supports
// them, otherwise a production checkpoint. rct reports whether the disks
// track changes. With requireApp only application-consistent checkpoints
// count: anything else is removed and ErrNotAppConsistent returned.
//
// Cancelling the backup does not interrupt a checkpoint being created:
// Hyper-V finishes the job anyway, and a checkpoint not yet renamed to its
// backupzit_ name would be removed by nothing (CleanupCheckpoints only knows
// that prefix), leaving the VM on growing differencing disks for good. So
// creating runs to the end, and a checkpoint finished after the cancel is
// removed right away.
func takeCheckpoint(ctx context.Context, vm vmInfo, name string, requireApp, crashOK bool) (ci checkpointInfo, consistency string, rct bool, warns []string, err error) {
	run, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	stop := context.AfterFunc(ctx, func() { time.AfterFunc(checkpointGrace, cancel) })
	defer stop()
	ci, consistency, rct, warns, err = createCheckpoint(ctx, run, vm, name, requireApp, crashOK)
	if err != nil && ctx.Err() != nil && run.Err() != nil {
		// The grace ran out while PowerShell created it: Hyper-V may still
		// add the checkpoint under its default name. The ledger item says
		// so; the agent's janitor removes it (S-2).
		it := checkpointItem(vm, name)
		it.Pending = true
		leftover.Add(ctx, it)
		warns = append(warns, fmt.Sprintf("the backup was cancelled while Hyper-V created a checkpoint, which did not finish within %s; the agent removes it when it appears (the dashboard lists it until then)", checkpointGrace))
		return ci, consistency, rct, warns, err
	}
	if err != nil || ctx.Err() != nil {
		// Created after all (a cancel during the creation) or half made:
		// removed with its own time, the grace may be spent.
		rm, rmCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
		defer rmCancel()
		if rerr := removeCheckpointVerified(rm, vm, name, nil); rerr != nil && !errors.Is(rerr, errMoved) {
			warns = append(warns, "remove checkpoint "+name+": "+rerr.Error()+"; the agent tries again and the dashboard lists it until it is gone")
		} else if errors.Is(rerr, errMoved) {
			leftover.Done(checkpointItem(vm, name))
		}
		if err == nil {
			err = ctx.Err()
		}
		return checkpointInfo{}, "", false, warns, err
	}
	return ci, consistency, rct, warns, nil
}

// createCheckpoint does the work of takeCheckpoint. PowerShell runs under
// run; ctx only stops it from starting another attempt.
func createCheckpoint(ctx, run context.Context, vm vmInfo, name string, requireApp, crashOK bool) (ci checkpointInfo, consistency string, rct bool, warns []string, err error) {
	vars := `$id = ` + psq(vm.ID) + `; $name = ` + psq(name) + `; `
	levels := []string{"1", "2"}
	if requireApp {
		levels = levels[:1]
	}
	var appErr error
	for _, level := range levels {
		if err := ctx.Err(); err != nil {
			return ci, "", false, warns, err
		}
		out, err := ps(run, vars+`$level = `+level+"\n"+recoveryScript)
		if err == nil && json.Unmarshal(out, &ci) == nil && len(ci.Disks) > 0 {
			if level == "1" {
				return ci, "recovery checkpoint, application-consistent (VSS or file system freeze inside the guest)", true, warns, nil
			}
			if crashOK {
				return ci, "recovery checkpoint, crash-consistent (no backup integration in the guest; expected for this VM)", true, warns, nil
			}
			return ci, "recovery checkpoint, crash-consistent (no backup integration in the guest)", true, warns, nil
		}
		if err == nil {
			err = errors.New("no disks in the recovery checkpoint")
		}
		switch {
		case level == "1" && requireApp:
			appErr = err
		case level == "1" && crashOK:
			// Listed in the job as a VM that cannot do better: no warning.
		case level == "1":
			warns = append(warns, "application-consistent checkpoint failed ("+lastLine(err.Error())+"), took a crash-consistent one")
		default:
			warns = append(warns[:0], "recovery checkpoints are not available ("+lastLine(err.Error())+"); every backup reads the whole disks")
		}
	}
	// Older hosts: a production checkpoint without change tracking.
	if err := ctx.Err(); err != nil {
		return ci, "", false, warns, err
	}
	sel := `$vm = Get-VM -Id ` + psq(vm.ID) + `; `
	out, err := ps(run, sel+`Checkpoint-VM -VM $vm -SnapshotName `+psq(name)+`; $s = Get-VMSnapshot -VM $vm -Name `+psq(name)+
		`; [pscustomobject]@{ type="$($s.SnapshotType)"; state="$($s.State)"; disks=@(Get-VMHardDiskDrive -VMSnapshot $s | ForEach-Object { [pscustomobject]@{ type="$($_.ControllerType)"; num=$_.ControllerNumber; loc=$_.ControllerLocation; path=$_.Path } }) } | ConvertTo-Json -Depth 4 -Compress`)
	if err != nil {
		if requireApp {
			return ci, "", false, warns, fmt.Errorf("%w (%s)", ErrNotAppConsistent, lastLine(err.Error()))
		}
		return ci, "", false, warns, fmt.Errorf("create checkpoint: %w", err)
	}
	if err := json.Unmarshal(out, &ci); err != nil || len(ci.Disks) == 0 {
		return ci, "", false, warns, fmt.Errorf("read checkpoint disks: %v", err)
	}
	consistency = "standard checkpoint, crash-consistent (no backup integration in the guest)"
	if strings.EqualFold(ci.Type, "Production") || strings.EqualFold(ci.State, "Off") {
		consistency = "production checkpoint (VSS or file system freeze inside the guest)"
	} else if requireApp {
		// Hyper-V fell back to a standard checkpoint: not what the job wants.
		_, _ = ps(run, sel+`Get-VMSnapshot -VM $vm -Name `+psq(name)+` | Remove-VMSnapshot`)
		why := "only a crash-consistent checkpoint was possible"
		if appErr != nil {
			why = lastLine(appErr.Error())
		}
		return ci, "", false, warns, fmt.Errorf("%w (%s); check the Backup (volume shadow copy) integration service and VSS inside the guest", ErrNotAppConsistent, why)
	}
	return ci, consistency, false, warns, nil
}

func lastLine(s string) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(l[len(l)-1])
}

func backupVM(ctx context.Context, r *repo.Repository, vm vmInfo, sn *repo.Snapshot, prev map[string]diskimg.Previous, progress func(uint64), requireApp, crashOK bool, excludeDisks []string, log func(string, ...any)) (_ *repo.Guest, warns []string, _ error) {
	if len(vm.Disks) == 0 {
		return nil, nil, errNoDisks
	}
	// Disks the job leaves out, by controller location (a checkpoint reports
	// the same locations with other file names).
	excluded := map[string]string{}
	for _, d := range vm.Disks {
		if DiskExcluded(excludeDisks, d.Path) {
			excluded[d.key()] = d.Path
		}
	}
	if len(excluded) == len(vm.Disks) {
		return nil, nil, errors.New("every disk of the VM is excluded by the job (Exclude disks); the VM was not backed up")
	}
	unlock, err := lockVM(ctx, vm.ID, true)
	if err != nil {
		return nil, nil, err
	}
	defer unlock()
	cfg, _ := json.Marshal(vm)
	g := &repo.Guest{Platform: "hyperv", Type: "vm", VMID: VMIDFor(vm.ID), ID: vm.ID, Name: vm.Name, Running: vm.State != "Off", Config: string(cfg)}
	disks := vm.Disks
	rct := false
	if vm.State == "Off" {
		g.Consistency = "VM was off, disks read directly"
	} else {
		// A prefixed checkpoint may still belong to another process or to a
		// backup reading after live migration. Only remove our own checkpoint.
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return nil, nil, fmt.Errorf("name checkpoint: %w", err)
		}
		name := snapPrefix + hex.EncodeToString(nonce[:])
		// Recorded before it exists: a crash during the creation leaves a
		// record the janitor finds it by (S-1).
		leftover.Add(ctx, checkpointItem(vm, name))
		ci, consistency, withRCT, w, err := takeCheckpoint(ctx, vm, name, requireApp, crashOK)
		warns = append(warns, w...)
		if err != nil {
			if vmGone(err) {
				return nil, nil, errors.New("the VM moved to another node before its checkpoint (live migration); it was not backed up in this run - the node that runs it now backs it up in its next run")
			}
			return nil, warns, err
		}
		defer func() {
			c, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			err := removeCheckpointVerified(c, vm, name, leftover.RunWaits)
			switch {
			case errors.Is(err, errMoved):
				leftover.Done(checkpointItem(vm, name)) // the node that runs it now removes it
			case err != nil:
				warns = append(warns, "remove checkpoint "+name+": "+err.Error()+"; the agent tries again and the dashboard lists it until it is gone")
			}
			switch {
			case errors.Is(err, errMoved):
				// Live-migrated to another node during the backup: that node's
				// agent removes the checkpoint once this marker says the
				// backup has finished reading (CleanupCheckpoints).
				if m := doneMarker(vm, name); m == "" || os.WriteFile(m, nil, 0o600) != nil {
					warns = append(warns, "the VM moved to another node during the backup; remove its checkpoint "+name+" there (Hyper-V Manager → Checkpoints)")
				} else {
					warns = append(warns, "the VM moved to another node during the backup; the agent there removes its checkpoint "+name)
				}
			}
		}()
		disks, g.Consistency, rct = ci.Disks, consistency, withRCT
		// The checkpoint leaves out disks Hyper-V cannot checkpoint (pass-
		// through disks, shared VHD Sets): say so, and record them like
		// excluded disks, so that an in-place restore keeps them attached
		// instead of detaching them (M-17).
		inCheckpoint := map[string]bool{}
		for _, d := range ci.Disks {
			inCheckpoint[d.key()] = true
		}
		for _, d := range vm.Disks {
			if _, ok := excluded[d.key()]; ok || inCheckpoint[d.key()] {
				continue
			}
			excluded[d.key()] = d.Path
			warns = append(warns, fmt.Sprintf("disk %s (%s) cannot be part of a checkpoint (a pass-through disk or a shared VHD Set) and is not in the backup; an in-place restore keeps it attached", d.key(), d.Path))
		}
	}
	for _, d := range vm.Disks {
		if p, ok := excluded[d.key()]; ok {
			g.ExcludedDisks = append(g.ExcludedDisks, repo.ExcludedDisk{Key: d.key(), Path: p})
			log("disk excluded by the job", "vm", vm.Name, "disk", d.key(), "path", p)
		}
	}
	for _, d := range disks {
		if _, ok := excluded[d.key()]; ok {
			continue
		}
		gd, err := backupDisk(ctx, r, vm, d, rct, prev, sn, progress, log)
		if err != nil {
			return nil, warns, fmt.Errorf("%s: %w", d.key(), err)
		}
		g.Disks = append(g.Disks, gd)
	}
	if vm.Generation == 2 && vm.TPM {
		if err := saveTPM(ctx, r, &vm); err != nil {
			warns = append(warns, "vTPM state not saved ("+lastLine(err.Error())+"); a restored VM gets a new, empty vTPM")
		} else {
			cfg, _ := json.Marshal(vm)
			g.Config = string(cfg)
		}
	}
	return g, warns, nil
}

// backupDisk stores one disk; with change tracking only what changed since
// the previous backup is read.
func backupDisk(ctx context.Context, r *repo.Repository, vm vmInfo, d vmDisk, rct bool, prev map[string]diskimg.Previous, sn *repo.Snapshot, progress func(uint64), log func(string, ...any)) (repo.GuestDisk, error) {
	ad, err := attachVHD(d.Path, true)
	if err != nil {
		return repo.GuestDisk{}, err
	}
	defer ad.close()
	var previous *diskimg.Previous
	if pd, ok := prev[diskimg.PreviousKey(vm.ID, d.key())]; ok {
		previous = &pd
	}
	plan := planDiskRead(ad.size, rct, previous, ad)
	plan.log(log, vm.ID, d.key(), ad.size)
	f, err := os.Open(ad.path)
	if err != nil {
		return repo.GuestDisk{}, err
	}
	defer f.Close()
	src := "checkpoint"
	if vm.State == "Off" {
		src = "offline"
	}
	var bytesRead uint64
	img, err := diskimg.StoreAreas(ctx, r, f, ad.size, plan.areas, plan.base, len(sn.Images), fmt.Sprintf("hyperv %s %s %s (%s)", vm.Name, d.key(), d.Path, plan.method), src, func(n uint64) {
		bytesRead += n
		if progress != nil {
			progress(n)
		}
	})
	if err != nil {
		return repo.GuestDisk{}, err
	}
	if log != nil {
		log("hyperv disk read complete", "vm_id", vm.ID, "disk", d.key(), "mode", plan.mode(), "reason", plan.reason, "disk_bytes", ad.size, "bytes_read", bytesRead)
	}
	sn.Images = append(sn.Images, img)
	return repo.GuestDisk{Key: d.key(), Volume: d.Path, Storage: dirOf(d.Path), Format: formatOf(d.Path),
		Size: img.Size, Image: len(sn.Images) - 1, ChangeID: plan.changeID}, nil
}

func dirOf(p string) string {
	if i := strings.LastIndexAny(p, `\/`); i > 0 {
		return p[:i]
	}
	return ""
}

func formatOf(p string) string {
	p = strings.ToLower(p)
	switch {
	case strings.HasSuffix(p, ".vhd"):
		return "vhd"
	case strings.HasSuffix(p, ".avhdx"):
		return "avhdx"
	}
	return "vhdx"
}
