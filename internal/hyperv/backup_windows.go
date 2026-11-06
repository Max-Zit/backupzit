package hyperv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/diskimg"
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
	Progress             func(done, total uint64)
	Log                  func(msg string, args ...any)
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
		g, warns, err := backupVM(ctx, r, vm, sn, prev, progress, opts.RequireAppConsistent)
		for _, w := range warns {
			addErr(vm.Name, errors.New(w))
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if errors.Is(err, ErrNotAppConsistent) {
				notApp++
			}
			addErr(vm.Name, err)
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

// takeCheckpoint creates the backup's checkpoint: a recovery checkpoint
// (application-consistent, else crash-consistent) where Hyper-V supports
// them, otherwise a production checkpoint. rct reports whether the disks
// track changes. With requireApp only application-consistent checkpoints
// count: anything else is removed and ErrNotAppConsistent returned.
func takeCheckpoint(ctx context.Context, vm vmInfo, name string, requireApp bool) (ci checkpointInfo, consistency string, rct bool, warns []string, err error) {
	vars := `$id = ` + psq(vm.ID) + `; $name = ` + psq(name) + `; `
	levels := []string{"1", "2"}
	if requireApp {
		levels = levels[:1]
	}
	var appErr error
	for _, level := range levels {
		out, err := ps(ctx, vars+`$level = `+level+"\n"+recoveryScript)
		if err == nil && json.Unmarshal(out, &ci) == nil && len(ci.Disks) > 0 {
			if level == "1" {
				return ci, "recovery checkpoint, application-consistent (VSS or file system freeze inside the guest)", true, warns, nil
			}
			return ci, "recovery checkpoint, crash-consistent (no backup integration in the guest)", true, warns, nil
		}
		if err == nil {
			err = errors.New("no disks in the recovery checkpoint")
		}
		switch {
		case level == "1" && requireApp:
			appErr = err
		case level == "1":
			warns = append(warns, "application-consistent checkpoint failed ("+lastLine(err.Error())+"), took a crash-consistent one")
		default:
			warns = append(warns[:0], "recovery checkpoints are not available ("+lastLine(err.Error())+"); every backup reads the whole disks")
		}
	}
	// Older hosts: a production checkpoint without change tracking.
	sel := `$vm = Get-VM -Id ` + psq(vm.ID) + `; `
	out, err := ps(ctx, sel+`Checkpoint-VM -VM $vm -SnapshotName `+psq(name)+`; $s = Get-VMSnapshot -VM $vm -Name `+psq(name)+
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
		_, _ = ps(ctx, sel+`Get-VMSnapshot -VM $vm -Name `+psq(name)+` | Remove-VMSnapshot`)
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

func backupVM(ctx context.Context, r *repo.Repository, vm vmInfo, sn *repo.Snapshot, prev map[string]diskimg.Previous, progress func(uint64), requireApp bool) (_ *repo.Guest, warns []string, _ error) {
	if len(vm.Disks) == 0 {
		return nil, nil, errNoDisks
	}
	cfg, _ := json.Marshal(vm)
	g := &repo.Guest{Platform: "hyperv", Type: "vm", VMID: VMIDFor(vm.ID), ID: vm.ID, Name: vm.Name, Running: vm.State != "Off", Config: string(cfg)}
	disks := vm.Disks
	rct := false
	if vm.State == "Off" {
		g.Consistency = "VM was off, disks read directly"
	} else {
		sel := `$vm = Get-VM -Id ` + psq(vm.ID) + `; `
		// Remove checkpoints left behind by an interrupted backup.
		_, _ = ps(ctx, sel+`Get-VMSnapshot -VM $vm | Where-Object Name -like '`+snapPrefix+`*' | Remove-VMSnapshot`)
		name := snapPrefix + strconv.FormatInt(time.Now().Unix(), 10)
		ci, consistency, withRCT, w, err := takeCheckpoint(ctx, vm, name, requireApp)
		warns = append(warns, w...)
		if err != nil {
			if vmGone(err) {
				return nil, nil, errors.New("the VM moved to another node before its checkpoint (live migration); it was not backed up in this run - the node that runs it now backs it up in its next run")
			}
			return nil, warns, err
		}
		defer func() {
			c, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			out, err := ps(c, `$vm = Get-VM -Id `+psq(vm.ID)+` -ErrorAction SilentlyContinue; if (-not $vm) { 'moved' } else { Get-VMSnapshot -VM $vm -Name `+psq(name)+` -ErrorAction Stop | Remove-VMSnapshot -ErrorAction Stop }`)
			switch {
			case err != nil:
				warns = append(warns, "remove checkpoint "+name+": "+lastLine(err.Error()))
			case strings.TrimSpace(string(out)) == "moved":
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
	}
	for _, d := range disks {
		gd, err := backupDisk(ctx, r, vm, d, rct, prev, sn, progress)
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
func backupDisk(ctx context.Context, r *repo.Repository, vm vmInfo, d vmDisk, rct bool, prev map[string]diskimg.Previous, sn *repo.Snapshot, progress func(uint64)) (repo.GuestDisk, error) {
	ad, err := attachVHD(d.Path, true)
	if err != nil {
		return repo.GuestDisk{}, err
	}
	defer ad.close()
	areas := []diskimg.Area{{Offset: 0, Length: ad.size}}
	var base *repo.DiskImage
	method, changeID := "whole disk", ""
	if rct {
		if on, id, err := ad.rctState(); err == nil && on {
			changeID = id
			if pd, ok := prev[diskimg.PreviousKey(vm.ID, d.key())]; ok && pd.Image.Size == ad.size && pd.ChangeID != "" {
				if ranges, err := ad.rctChanges(pd.ChangeID); err == nil {
					areas, base, method = areas[:0], pd.Image, "changed blocks"
					for _, rg := range ranges {
						areas = append(areas, diskimg.Area{Offset: rg[0], Length: rg[1]})
					}
				}
			}
		}
	}
	f, err := os.Open(ad.path)
	if err != nil {
		return repo.GuestDisk{}, err
	}
	defer f.Close()
	src := "checkpoint"
	if vm.State == "Off" {
		src = "offline"
	}
	img, err := diskimg.StoreAreas(ctx, r, f, ad.size, areas, base, len(sn.Images), fmt.Sprintf("hyperv %s %s %s (%s)", vm.Name, d.key(), d.Path, method), src, progress)
	if err != nil {
		return repo.GuestDisk{}, err
	}
	sn.Images = append(sn.Images, img)
	return repo.GuestDisk{Key: d.key(), Volume: d.Path, Storage: dirOf(d.Path), Format: formatOf(d.Path),
		Size: img.Size, Image: len(sn.Images) - 1, ChangeID: changeID}, nil
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
