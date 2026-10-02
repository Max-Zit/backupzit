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

	"github.com/backupzit/backupzit/internal/diskimg"
	"github.com/backupzit/backupzit/internal/repo"
)

// BackupOptions select the VMs to back up.
type BackupOptions struct {
	// VMIDs lists VMs by their BackupZit number (VMIDFor); nil means all.
	VMIDs    []int
	Exclude  []int
	Hostname string
	Version  string
	Tags     []string
	Progress func(done, total uint64)
	Log      func(msg string, args ...any)
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
	var sel []vmInfo
	if opts.VMIDs == nil {
		excl := map[int]bool{}
		for _, id := range opts.Exclude {
			excl[id] = true
		}
		for _, vm := range vms {
			if !excl[VMIDFor(vm.ID)] {
				sel = append(sel, vm)
			}
		}
	} else {
		for _, id := range opts.VMIDs {
			vm, ok := byID[id]
			if !ok {
				addErr(fmt.Sprintf("VM %d", id), errors.New("does not exist on this host (any more)"))
				continue
			}
			sel = append(sel, vm)
		}
	}
	if len(sel) == 0 && len(stats.Errors) == 0 {
		return nil, errors.New("no virtual machines on this Hyper-V host")
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
	for _, vm := range sel {
		opts.Log("backing up VM", "name", vm.Name, "id", vm.ID)
		g, warns, err := backupVM(ctx, r, vm, sn, progress)
		for _, w := range warns {
			addErr(vm.Name, errors.New(w))
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			addErr(vm.Name, err)
			continue
		}
		sn.Guests = append(sn.Guests, *g)
		sn.Paths = append(sn.Paths, "hyperv/"+vm.ID)
		stats.Files++
	}
	if len(sn.Guests) == 0 {
		return nil, fmt.Errorf("no VM could be backed up: %s", strings.Join(stats.Errors, "; "))
	}
	for _, img := range sn.Images {
		stats.Bytes += img.Partitions[0].StoredBytes
		stats.BytesRead += img.Size
	}
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

type checkpointInfo struct {
	Type string `json:"type"`
	// State is "Off" for production checkpoints (no saved memory) of a
	// running VM; Hyper-V labels both kinds "Standard".
	State string   `json:"state"`
	Disks []vmDisk `json:"disks"`
}

func backupVM(ctx context.Context, r *repo.Repository, vm vmInfo, sn *repo.Snapshot, progress func(uint64)) (*repo.Guest, []string, error) {
	if len(vm.Disks) == 0 {
		return nil, nil, errNoDisks
	}
	cfg, _ := json.Marshal(vm)
	g := &repo.Guest{Platform: "hyperv", Type: "vm", VMID: VMIDFor(vm.ID), ID: vm.ID, Name: vm.Name, Running: vm.State != "Off", Config: string(cfg)}
	disks := vm.Disks
	if vm.State == "Off" {
		g.Consistency = "VM was off, disks read directly"
	} else {
		sel := `$vm = Get-VM -Id ` + psq(vm.ID) + `; `
		// Remove checkpoints left behind by an interrupted backup.
		ps(ctx, sel+`Get-VMSnapshot -VM $vm | Where-Object Name -like '`+snapPrefix+`*' | Remove-VMSnapshot`)
		name := snapPrefix + strconv.FormatInt(time.Now().Unix(), 10)
		out, err := ps(ctx, sel+`Checkpoint-VM -VM $vm -SnapshotName `+psq(name)+`; $s = Get-VMSnapshot -VM $vm -Name `+psq(name)+
			`; [pscustomobject]@{ type="$($s.SnapshotType)"; state="$($s.State)"; disks=@(Get-VMHardDiskDrive -VMSnapshot $s | ForEach-Object { [pscustomobject]@{ type="$($_.ControllerType)"; num=$_.ControllerNumber; loc=$_.ControllerLocation; path=$_.Path } }) } | ConvertTo-Json -Depth 4 -Compress`)
		if err != nil {
			return nil, nil, fmt.Errorf("create checkpoint: %w", err)
		}
		defer func() {
			c, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			ps(c, sel+`Get-VMSnapshot -VM $vm -Name `+psq(name)+` | Remove-VMSnapshot`)
		}()
		var ci checkpointInfo
		if err := json.Unmarshal(out, &ci); err != nil || len(ci.Disks) == 0 {
			return nil, nil, fmt.Errorf("read checkpoint disks: %v", err)
		}
		disks = ci.Disks
		if strings.EqualFold(ci.Type, "Production") || strings.EqualFold(ci.State, "Off") {
			g.Consistency = "production checkpoint (VSS or file system freeze inside the guest)"
		} else {
			g.Consistency = "standard checkpoint, crash-consistent (no backup integration in the guest)"
		}
	}
	for _, d := range disks {
		ad, err := attachVHD(d.Path, true)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", d.key(), err)
		}
		f, err := os.Open(ad.path)
		if err != nil {
			ad.close()
			return nil, nil, fmt.Errorf("%s: %w", d.key(), err)
		}
		src := "checkpoint"
		if vm.State == "Off" {
			src = "offline"
		}
		img, err := diskimg.Store(ctx, r, f, ad.size, len(sn.Images), fmt.Sprintf("hyperv %s %s %s", vm.Name, d.key(), d.Path), src, progress)
		f.Close()
		ad.close()
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", d.key(), err)
		}
		sn.Images = append(sn.Images, img)
		g.Disks = append(g.Disks, repo.GuestDisk{Key: d.key(), Volume: d.Path, Storage: dirOf(d.Path), Format: formatOf(d.Path),
			Size: img.Size, Image: len(sn.Images) - 1})
	}
	return g, nil, nil
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
