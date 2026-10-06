package hyperv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/diskimg"
	"github.com/max-zit/backupzit/internal/repo"
)

// RestoreOptions control a VM restore.
type RestoreOptions struct {
	// VMID selects the VM in the snapshot (VMIDFor its GUID).
	VMID int
	// AsNew creates a second VM next to the original (new name and MAC
	// addresses); otherwise the VM is recreated under its own name.
	AsNew bool
	Name  string
	// Folder for the new virtual disks ("" = the host's default).
	Folder string
	// Overwrite replaces the original VM if it still exists: it is turned
	// off and removed; its disk files are renamed, not deleted.
	Overwrite bool
	Start     bool
	Progress  func(done, total uint64)
	Log       func(msg string, args ...any)
}

// RestoreResult describes the restored VM.
type RestoreResult struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Host    string   `json:"host"`
	Disks   []string `json:"disks"`
	Bytes   uint64   `json:"bytes"`
	Started bool     `json:"started,omitempty"`
	NewMACs bool     `json:"new_macs,omitempty"`
	Notes   []string `json:"notes,omitempty"`
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9 ._()-]+`)

var diskKeyRe = regexp.MustCompile(`^(ide|scsi)(\d+):(\d+)$`)

// Restore recreates a VM of a Hyper-V backup on this host.
func Restore(ctx context.Context, r *repo.Repository, sn *repo.Snapshot, opts RestoreOptions) (*RestoreResult, error) {
	if opts.Log == nil {
		opts.Log = func(string, ...any) {}
	}
	var g *repo.Guest
	for i := range sn.Guests {
		if sn.Guests[i].VMID == opts.VMID && sn.Guests[i].Platform == "hyperv" {
			g = &sn.Guests[i]
		}
	}
	if g == nil {
		return nil, fmt.Errorf("snapshot %s has no Hyper-V VM %d", sn.ID.Short(), opts.VMID)
	}
	var cfg vmInfo
	if err := json.Unmarshal([]byte(g.Config), &cfg); err != nil {
		return nil, fmt.Errorf("VM settings in the backup: %w", err)
	}
	host, _ := os.Hostname()
	res := &RestoreResult{Host: host}
	existing, _ := getVM(ctx, g.ID)
	name := strings.TrimSpace(opts.Name)
	switch {
	case name != "":
	case opts.AsNew:
		name = cfg.Name + " (restored " + time.Now().Format("2006-01-02 15-04") + ")"
	default:
		name = cfg.Name
	}
	clone := opts.AsNew && existing != nil
	// In place: the existing VM (same ID, or the same name after it was
	// recreated) keeps its identity, network adapters and MAC addresses;
	// only its disks and CPU/memory settings are replaced.
	inPlace := false
	if !opts.AsNew && existing == nil {
		existing = findByName(ctx, cfg.Name)
	}
	if !opts.AsNew && existing != nil {
		if !opts.Overwrite {
			return nil, fmt.Errorf("VM %q still exists; restore it as a new VM or choose to replace it", existing.Name)
		}
		opts.Log("restoring in place", "name", existing.Name)
		out, err := ps(ctx, `$vm = Get-VM -Id `+psq(existing.ID)+`; Stop-VM -VM $vm -TurnOff -Force -ErrorAction SilentlyContinue; `+
			`Get-VMSnapshot -VM $vm | Remove-VMSnapshot -IncludeAllChildSnapshots -ErrorAction SilentlyContinue; $t = 0; `+
			`while ("$((Get-VM -Id `+psq(existing.ID)+`).Status)" -match 'Merg' -and $t -lt 3600) { Start-Sleep 2; $t += 2 }; `+
			`$p = @(Get-VMHardDiskDrive -VM $vm | ForEach-Object { $_.Path }); Get-VMHardDiskDrive -VM $vm | Remove-VMHardDiskDrive; $p -join "`+"`n"+`"`)
		if err != nil {
			return nil, fmt.Errorf("prepare the existing VM: %w", err)
		}
		stamp := time.Now().Format("20060102-150405")
		for _, p := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if p = strings.TrimSpace(p); p == "" {
				continue
			}
			if opts.Folder == "" {
				opts.Folder = dirOf(p)
			}
			if err := os.Rename(p, p+".replaced-"+stamp); err == nil {
				res.Notes = append(res.Notes, fmt.Sprintf("old disk kept as %s.replaced-%s - delete it when the restored VM works", p, stamp))
			}
		}
		inPlace, name, clone = true, existing.Name, false
	}
	folder := strings.TrimSpace(opts.Folder)
	if folder == "" {
		out, err := ps(ctx, `(Get-VMHost).VirtualHardDiskPath`)
		if err != nil {
			return nil, err
		}
		folder = strings.TrimSpace(string(out))
	}
	dir := filepath.Join(folder, strings.TrimSpace(unsafeName.ReplaceAllString(name, "_")))
	if inPlace {
		dir = folder // next to the original disks
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}

	var total, done uint64
	for _, d := range g.Disks {
		total += d.Size
	}
	progress := func(n uint64) {
		done += n
		if opts.Progress != nil {
			opts.Progress(done, max(total, done))
		}
	}
	var created []string
	ok := false
	defer func() {
		if !ok {
			for _, f := range created {
				os.Remove(f)
			}
		}
	}()
	type placed struct {
		disk vmDisk
		path string
	}
	var disks []placed
	for _, d := range g.Disks {
		if d.Image < 0 || d.Image >= len(sn.Images) {
			return nil, fmt.Errorf("%s: disk image missing in the backup", d.Key)
		}
		vd := vmDisk{Type: "SCSI", Location: len(disks)}
		if m := diskKeyRe.FindStringSubmatch(d.Key); m != nil {
			vd.Type = strings.ToUpper(m[1])
			vd.Number, _ = strconv.Atoi(m[2])
			vd.Location, _ = strconv.Atoi(m[3])
		}
		file := filepath.Join(dir, strings.ReplaceAll(d.Key, ":", "-")+".vhdx")
		if _, err := os.Stat(file); err == nil {
			file = filepath.Join(dir, strings.ReplaceAll(d.Key, ":", "-")+"-"+strconv.FormatInt(time.Now().Unix(), 10)+".vhdx")
		}
		opts.Log("writing disk", "key", d.Key, "file", file)
		if _, err := ps(ctx, `New-VHD -Path `+psq(file)+` -SizeBytes `+strconv.FormatUint(d.Size, 10)+` -Dynamic | Out-Null`); err != nil {
			return nil, fmt.Errorf("create %s: %w", file, err)
		}
		created = append(created, file)
		if err := writeVHD(ctx, r, &sn.Images[d.Image], file, progress); err != nil {
			return nil, fmt.Errorf("%s: %w", d.Key, err)
		}
		disks = append(disks, placed{vd, file})
		res.Disks = append(res.Disks, d.Key+" -> "+file)
		res.Bytes += d.Size
	}

	// Create the VM (or update the existing one) with the backed up settings.
	var s strings.Builder
	gen := max(cfg.Generation, 1)
	if inPlace {
		fmt.Fprintf(&s, `$vm = Get-VM -Id %s; `, psq(existing.ID))
		gen = existing.Generation
	} else {
		fmt.Fprintf(&s, `$vm = New-VM -Name %s -Generation %d -MemoryStartupBytes %d -NoVHD; `, psq(name), gen, max(cfg.Memory, 512<<20))
		// Windows turns automatic checkpoints on for new VMs; every start
		// would then leave an .avhdx next to the restored disk.
		s.WriteString(`if ((Get-Command Set-VM).Parameters.ContainsKey('AutomaticCheckpointsEnabled')) { Set-VM -VM $vm -AutomaticCheckpointsEnabled $false }; `)
	}
	if cfg.CPU > 0 {
		fmt.Fprintf(&s, `Set-VMProcessor -VM $vm -Count %d; `, cfg.CPU)
	}
	if cfg.Dynamic && cfg.MemMin > 0 && cfg.MemMax >= cfg.Memory {
		fmt.Fprintf(&s, `Set-VMMemory -VM $vm -DynamicMemoryEnabled $true -MinimumBytes %d -MaximumBytes %d -StartupBytes %d; `, cfg.MemMin, cfg.MemMax, cfg.Memory)
	} else if inPlace && cfg.Memory > 0 {
		fmt.Fprintf(&s, `Set-VMMemory -VM $vm -DynamicMemoryEnabled $false -StartupBytes %d; `, cfg.Memory)
	}
	if !inPlace {
		s.WriteString(`Get-VMNetworkAdapter -VM $vm | Remove-VMNetworkAdapter; `)
	}
	for i, n := range cfg.NICs {
		if inPlace {
			break // the existing adapters stay
		}
		fmt.Fprintf(&s, `$sw = Get-VMSwitch -Name %s -ErrorAction SilentlyContinue; `, psq(n.Switch))
		mac := ""
		// A restore in place keeps the MAC address (also a dynamically assigned
		// one), so the guest keeps its network identity; copies get new ones.
		if !clone && n.MAC != "" {
			mac = ` -StaticMacAddress ` + psq(n.MAC)
		}
		fmt.Fprintf(&s, `if ($sw) { $nic = Add-VMNetworkAdapter -VM $vm -Name 'Network Adapter %d' -SwitchName %s%s -Passthru } else { $nic = Add-VMNetworkAdapter -VM $vm -Name 'Network Adapter %d'%s -Passthru; 'NOSWITCH:' + %s }; `,
			i+1, psq(n.Switch), mac, i+1, mac, psq(n.Switch))
		if n.VLAN > 0 {
			fmt.Fprintf(&s, `Set-VMNetworkAdapterVlan -VMNetworkAdapter $nic -Access -VlanId %d; `, n.VLAN)
		}
	}
	for _, d := range disks {
		if d.disk.Type == "SCSI" {
			fmt.Fprintf(&s, `while (@(Get-VMScsiController -VM $vm).Count -le %d) { Add-VMScsiController -VM $vm }; `, d.disk.Number)
		}
		fmt.Fprintf(&s, `Add-VMHardDiskDrive -VM $vm -ControllerType %s -ControllerNumber %d -ControllerLocation %d -Path %s; `,
			d.disk.Type, d.disk.Number, d.disk.Location, psq(d.path))
	}
	if gen == 2 && !inPlace {
		sb := "Off"
		if strings.EqualFold(cfg.SecureBoot, "On") {
			sb = "On"
		}
		fmt.Fprintf(&s, `Set-VMFirmware -VM $vm -EnableSecureBoot %s; `, sb)
		if sb == "On" && cfg.Template != "" {
			fmt.Fprintf(&s, `Set-VMFirmware -VM $vm -SecureBootTemplate %s; `, psq(cfg.Template))
		}
		if cfg.TPM {
			s.WriteString(`Set-VMKeyProtector -VM $vm -NewLocalKeyProtector; Enable-VMTPM -VM $vm; `)
		}
	}
	if gen == 2 {
		s.WriteString(`Set-VMFirmware -VM $vm -FirstBootDevice (Get-VMHardDiskDrive -VM $vm | Select-Object -First 1); `)
	}
	if cfg.Checkpoint != "" {
		fmt.Fprintf(&s, `Set-VM -VM $vm -CheckpointType %s; `, psq(cfg.Checkpoint))
	}
	fmt.Fprintf(&s, `Set-VM -VM $vm -Notes %s; 'ID:' + $vm.Id.Guid`, psq(strings.TrimSpace(cfg.Notes+"\nRestored by BackupZit from the backup of "+sn.Time.Local().Format("2006-01-02 15:04"))))
	out, err := ps(ctx, s.String())
	if err != nil {
		if !inPlace {
			ps(context.Background(), `Get-VM -Name `+psq(name)+` -ErrorAction SilentlyContinue | Remove-VM -Force`)
		}
		return nil, fmt.Errorf("create the VM: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "ID:"):
			res.ID = strings.TrimPrefix(line, "ID:")
		case strings.HasPrefix(line, "NOSWITCH:"):
			res.Notes = append(res.Notes, fmt.Sprintf("virtual switch %q does not exist on this host; the network adapter is not connected", strings.TrimPrefix(line, "NOSWITCH:")))
		}
	}
	if res.ID == "" {
		return nil, errors.New("the new VM did not report its ID")
	}
	ok = true
	res.Name, res.NewMACs = name, clone
	if opts.Start {
		if _, err := ps(ctx, `Start-VM -VM (Get-VM -Id `+psq(res.ID)+`)`); err != nil {
			res.Notes = append(res.Notes, "VM restored but could not be started: "+err.Error())
		} else {
			res.Started = true
		}
	}
	return res, nil
}

// writeVHD writes a disk image into a new virtual disk file.
func writeVHD(ctx context.Context, r *repo.Repository, img *repo.DiskImage, file string, progress func(uint64)) error {
	ad, err := attachVHD(file, false)
	if err != nil {
		return err
	}
	defer ad.close()
	f, err := ad.openForWrite()
	if err != nil {
		return err
	}
	defer f.Close()
	if err := diskimg.Write(ctx, r, img, f, false, progress); err != nil {
		return err
	}
	return f.Sync()
}

// findByName returns the only VM with this name, if there is exactly one.
func findByName(ctx context.Context, name string) *vmInfo {
	out, err := ps(ctx, `@(Get-VM | Where-Object Name -eq `+psq(name)+` | ForEach-Object { $_.Id.Guid })`)
	if err != nil {
		return nil
	}
	ids := strings.Fields(strings.TrimSpace(string(out)))
	if len(ids) != 1 {
		return nil
	}
	vm, err := getVM(ctx, ids[0])
	if err != nil {
		return nil
	}
	return vm
}
