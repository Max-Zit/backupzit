// Package hyperv backs up and restores Hyper-V virtual machines from the
// BackupZit agent on the Hyper-V host, without agents in the guests: a
// production checkpoint (VSS inside Windows guests, file system freeze in
// Linux guests with the Hyper-V daemons) makes the disks consistent, the
// disks are read through the Virtual Disk API, and restores create the VM
// again with the Hyper-V PowerShell module.
package hyperv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/backupzit/backupzit/internal/pve"
	"golang.org/x/sys/windows/svc/mgr"
)

// snapPrefix names the temporary checkpoints taken for a backup.
const snapPrefix = "backupzit_"

var guidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Available reports whether this machine is a Hyper-V host.
func Available() bool {
	m, err := mgr.Connect()
	if err != nil {
		return false
	}
	defer m.Disconnect()
	s, err := m.OpenService("vmms")
	if err != nil {
		return false
	}
	s.Close()
	return true
}

// VMIDFor derives the stable number BackupZit uses to select a Hyper-V VM
// (Hyper-V itself identifies VMs by GUID).
func VMIDFor(guid string) int {
	h := fnv.New32a()
	h.Write([]byte(strings.ToLower(guid)))
	return 1_000_000_000 + int(h.Sum32()%1_000_000_000)
}

// vmInfo is what PowerShell reports about a VM.
type vmInfo struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	State      string   `json:"state"`
	Generation int      `json:"generation"`
	Version    string   `json:"version"`
	CPU        int      `json:"cpu"`
	Memory     uint64   `json:"memory"`
	Dynamic    bool     `json:"dynamic"`
	MemMin     uint64   `json:"memmin"`
	MemMax     uint64   `json:"memmax"`
	SecureBoot string   `json:"secureboot"`
	Template   string   `json:"template"`
	TPM        bool     `json:"tpm"`
	Notes      string   `json:"notes"`
	NICs       []vmNIC  `json:"nics"`
	Disks      []vmDisk `json:"disks"`
	DiskSize   uint64   `json:"disksize"`
	Checkpoint string   `json:"checkpointtype"`
}

type vmNIC struct {
	Switch     string `json:"switch"`
	MAC        string `json:"mac"`
	DynamicMAC bool   `json:"dynamicmac"`
	VLAN       int    `json:"vlan"`
}

type vmDisk struct {
	Type     string `json:"type"` // IDE | SCSI
	Number   int    `json:"num"`
	Location int    `json:"loc"`
	Path     string `json:"path"`
	Size     uint64 `json:"size"`
}

func (d vmDisk) key() string {
	return fmt.Sprintf("%s%d:%d", strings.ToLower(d.Type), d.Number, d.Location)
}

// psVMInfo is the PowerShell expression that describes VM $vm.
const psVMInfo = `[pscustomobject]@{
 id=$vm.Id.Guid; name=$vm.Name; state="$($vm.State)"; generation=$vm.Generation; version="$($vm.Version)";
 cpu=$vm.ProcessorCount; memory=$vm.MemoryStartup; dynamic=$vm.DynamicMemoryEnabled; memmin=$vm.MemoryMinimum; memmax=$vm.MemoryMaximum;
 secureboot=$(if ($vm.Generation -eq 2) { "$((Get-VMFirmware -VM $vm).SecureBoot)" } else { "" });
 template=$(if ($vm.Generation -eq 2) { "$((Get-VMFirmware -VM $vm).SecureBootTemplate)" } else { "" });
 tpm=$(try { [bool](Get-VMSecurity -VM $vm).TpmEnabled } catch { $false });
 notes="$($vm.Notes)"; checkpointtype="$($vm.CheckpointType)";
 nics=@(Get-VMNetworkAdapter -VM $vm | ForEach-Object { [pscustomobject]@{ switch="$($_.SwitchName)"; mac=$_.MacAddress; dynamicmac=$_.DynamicMacAddressEnabled; vlan=[int](Get-VMNetworkAdapterVlan -VMNetworkAdapter $_).AccessVlanId } });
 disks=@(Get-VMHardDiskDrive -VM $vm | ForEach-Object { [pscustomobject]@{ type="$($_.ControllerType)"; num=$_.ControllerNumber; loc=$_.ControllerLocation; path=$_.Path; size=$(try { (Get-VHD -Path $_.Path).Size } catch { 0 }) } });
 disksize=[uint64]((Get-VMHardDiskDrive -VM $vm | ForEach-Object { try { (Get-VHD -Path $_.Path).Size } catch { 0 } } | Measure-Object -Sum).Sum)
}`

func listVMs(ctx context.Context) ([]vmInfo, error) {
	out, err := ps(ctx, `@(Get-VM | ForEach-Object { $vm = $_; `+psVMInfo+` }) | ConvertTo-Json -Depth 5 -Compress`)
	if err != nil {
		return nil, err
	}
	return decodeVMs(out)
}

func getVM(ctx context.Context, id string) (*vmInfo, error) {
	if !guidRe.MatchString(id) {
		return nil, fmt.Errorf("invalid VM id %q", id)
	}
	out, err := ps(ctx, `$vm = Get-VM -Id `+psq(id)+`; @(`+psVMInfo+`) | ConvertTo-Json -Depth 5 -Compress`)
	if err != nil {
		return nil, err
	}
	vms, err := decodeVMs(out)
	if err != nil || len(vms) != 1 {
		return nil, fmt.Errorf("VM %s not found", id)
	}
	return &vms[0], nil
}

// decodeVMs accepts a JSON array or a single object (PowerShell 5 unwraps
// one-element arrays).
func decodeVMs(out []byte) ([]vmInfo, error) {
	s := strings.TrimSpace(string(out))
	if s == "" {
		return nil, nil
	}
	if strings.HasPrefix(s, "{") {
		s = "[" + s + "]"
	}
	var vms []vmInfo
	if err := json.Unmarshal([]byte(s), &vms); err != nil {
		return nil, fmt.Errorf("parse VM list: %w", err)
	}
	return vms, nil
}

// GetInventory lists the VMs of this host in the shape the console uses
// for hypervisors.
func GetInventory(ctx context.Context) (*pve.Inventory, error) {
	vms, err := listVMs(ctx)
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	inv := &pve.Inventory{Platform: "hyperv", Node: host, Guests: []pve.Guest{}}
	if v, err := ps(ctx, `"Hyper-V on $((Get-CimInstance Win32_OperatingSystem).Caption)"`); err == nil {
		inv.Version = strings.TrimSpace(string(v))
	}
	for _, vm := range vms {
		status := "stopped"
		switch vm.State {
		case "Running":
			status = "running"
		case "Paused", "Saved":
			status = strings.ToLower(vm.State)
		}
		inv.Guests = append(inv.Guests, pve.Guest{VMID: VMIDFor(vm.ID), ID: vm.ID, Name: vm.Name, Type: "vm", Node: host,
			Status: status, MaxDisk: vm.DiskSize, MaxMem: vm.Memory})
	}
	sort.Slice(inv.Guests, func(i, j int) bool { return strings.ToLower(inv.Guests[i].Name) < strings.ToLower(inv.Guests[j].Name) })
	inv.Storage = storageFolders(ctx, vms)
	return inv, nil
}

// storageFolders are the folders offered for restored disks: the host's
// default virtual hard disk folder and the folders of existing VM disks.
func storageFolders(ctx context.Context, vms []vmInfo) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		p = strings.TrimRight(strings.TrimSpace(p), `\`)
		if p != "" && !seen[strings.ToLower(p)] {
			seen[strings.ToLower(p)] = true
			out = append(out, p)
		}
	}
	if v, err := ps(ctx, `(Get-VMHost).VirtualHardDiskPath`); err == nil {
		add(string(v))
	}
	for _, vm := range vms {
		for _, d := range vm.Disks {
			add(filepath.Dir(d.Path))
		}
	}
	return out
}

func waitGone(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

var errNoDisks = errors.New("the VM has no virtual hard disks")
