package vmware

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/backupzit/backupzit/internal/pve"
)

// VM describes a virtual machine.
type VM struct {
	Ref      types.ManagedObjectReference `json:"-"`
	MoID     string                       `json:"moid"`
	UUID     string                       `json:"uuid"` // instance UUID
	VMID     int                          `json:"vmid"`
	Name     string                       `json:"name"`
	GuestID  string                       `json:"guest_id"`
	Power    string                       `json:"power"` // poweredOn, poweredOff, suspended
	CPUs     int32                        `json:"cpus"`
	MemoryMB int32                        `json:"memory_mb"`
	Firmware string                       `json:"firmware"`
	VMX      string                       `json:"vmx"` // [datastore] dir/name.vmx
	CBT      bool                         `json:"cbt"`
	Tools    bool                         `json:"tools"` // VMware Tools running
	Template bool                         `json:"template,omitempty"`
	// Snapshots is true when the VM has snapshots of its own.
	Snapshots bool   `json:"snapshots,omitempty"`
	Disks     []Disk `json:"disks"`
	NICs      []NIC  `json:"nics"`
}

// Disk is a virtual disk of a VM.
type Disk struct {
	DevKey   int32  `json:"dev_key"`
	Key      string `json:"key"` // vmx device: scsi0:0, sata0:1, nvme0:0, ide0:0
	File     string `json:"file"`
	Capacity uint64 `json:"capacity"`
	Thin     bool   `json:"thin"`
	// Delta is true when File is a snapshot delta, not the base disk.
	Delta    bool   `json:"delta,omitempty"`
	ChangeID string `json:"change_id,omitempty"`
}

// NIC is a network adapter.
type NIC struct {
	Label   string `json:"label"`
	Network string `json:"network"`
	MAC     string `json:"mac"`
	Type    string `json:"type"`
}

// DiskSize is the total capacity of the VM's disks.
func (v VM) DiskSize() uint64 {
	var n uint64
	for _, d := range v.Disks {
		n += d.Capacity
	}
	return n
}

var vmProps = []string{"name", "config.instanceUuid", "config.uuid", "config.guestId", "config.hardware.numCPU", "config.hardware.memoryMB",
	"config.firmware", "config.changeTrackingEnabled", "config.template", "config.hardware.device", "summary.config.vmPathName",
	"runtime.powerState", "guest.toolsRunningStatus", "snapshot"}

func vmFromMO(m mo.VirtualMachine) VM {
	v := VM{Ref: m.Self, MoID: m.Self.Value, Name: m.Name, Power: string(m.Runtime.PowerState),
		VMX: m.Summary.Config.VmPathName, Tools: m.Guest != nil && m.Guest.ToolsRunningStatus == "guestToolsRunning",
		Snapshots: m.Snapshot != nil}
	if m.Config != nil {
		v.UUID = m.Config.InstanceUuid
		if v.UUID == "" { // VMs created without vCenter IDs: the BIOS UUID
			v.UUID = m.Config.Uuid
		}
		v.GuestID = m.Config.GuestId
		v.CPUs = m.Config.Hardware.NumCPU
		v.MemoryMB = m.Config.Hardware.MemoryMB
		v.Firmware = m.Config.Firmware
		v.Template = m.Config.Template
		v.CBT = m.Config.ChangeTrackingEnabled != nil && *m.Config.ChangeTrackingEnabled
		v.Disks, v.NICs = devices(m.Config.Hardware.Device)
	}
	v.VMID = VMIDFor(v.UUID)
	return v
}

// devices extracts disks and network adapters from a device list.
func devices(devs object.VirtualDeviceList) ([]Disk, []NIC) {
	ctrl := map[int32]string{}
	for _, d := range devs {
		switch c := d.(type) {
		case *types.VirtualIDEController:
			ctrl[c.Key] = fmt.Sprintf("ide%d", c.BusNumber)
		case *types.VirtualAHCIController:
			ctrl[c.Key] = fmt.Sprintf("sata%d", c.BusNumber)
		case *types.VirtualNVMEController:
			ctrl[c.Key] = fmt.Sprintf("nvme%d", c.BusNumber)
		case types.BaseVirtualSCSIController:
			sc := c.GetVirtualSCSIController()
			ctrl[sc.Key] = fmt.Sprintf("scsi%d", sc.BusNumber)
		}
	}
	var disks []Disk
	var nics []NIC
	for _, d := range devs {
		switch x := d.(type) {
		case *types.VirtualDisk:
			disk := Disk{DevKey: x.Key, Capacity: uint64(x.CapacityInBytes)}
			if x.UnitNumber != nil {
				disk.Key = fmt.Sprintf("%s:%d", ctrl[x.ControllerKey], *x.UnitNumber)
			}
			if b, ok := x.Backing.(*types.VirtualDiskFlatVer2BackingInfo); ok {
				disk.File, disk.ChangeID, disk.Delta = b.FileName, b.ChangeId, b.Parent != nil
				disk.Thin = b.ThinProvisioned != nil && *b.ThinProvisioned
			} else if b, ok := x.Backing.(types.BaseVirtualDeviceFileBackingInfo); ok {
				disk.File = b.GetVirtualDeviceFileBackingInfo().FileName
				disk.Delta = true // seSparse / other formats are not read directly
			}
			disks = append(disks, disk)
		case types.BaseVirtualEthernetCard:
			e := x.GetVirtualEthernetCard()
			n := NIC{MAC: e.MacAddress, Type: strings.TrimPrefix(fmt.Sprintf("%T", x), "*types.Virtual")}
			if e.DeviceInfo != nil {
				n.Label = e.DeviceInfo.GetDescription().Label
			}
			if b, ok := e.Backing.(*types.VirtualEthernetCardNetworkBackingInfo); ok {
				n.Network = b.DeviceName
			}
			nics = append(nics, n)
		}
	}
	sort.Slice(disks, func(i, j int) bool { return disks[i].Key < disks[j].Key })
	return disks, nics
}

// VMs lists the virtual machines of the host.
func (c *Client) VMs(ctx context.Context) ([]VM, error) {
	m := view.NewManager(c.vim)
	v, err := m.CreateContainerView(ctx, c.vim.ServiceContent.RootFolder, []string{"VirtualMachine"}, true)
	if err != nil {
		return nil, err
	}
	defer v.Destroy(ctx)
	var mos []mo.VirtualMachine
	if err := v.Retrieve(ctx, []string{"VirtualMachine"}, vmProps, &mos); err != nil {
		return nil, err
	}
	out := make([]VM, 0, len(mos))
	for _, x := range mos {
		out = append(out, vmFromMO(x))
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

// vm reloads one VM.
func (c *Client) vm(ctx context.Context, ref types.ManagedObjectReference) (VM, error) {
	var m mo.VirtualMachine
	if err := property.DefaultCollector(c.vim).RetrieveOne(ctx, ref, vmProps, &m); err != nil {
		return VM{}, err
	}
	return vmFromMO(m), nil
}

// Datastores lists the datastores of the host with free space.
func (c *Client) Datastores(ctx context.Context) ([]Datastore, error) {
	dss, err := c.find.DatastoreList(ctx, "*")
	if err != nil {
		return nil, err
	}
	var out []Datastore
	for _, d := range dss {
		var m mo.Datastore
		if err := d.Properties(ctx, d.Reference(), []string{"summary"}, &m); err != nil {
			return nil, err
		}
		if !m.Summary.Accessible {
			continue
		}
		out = append(out, Datastore{Name: m.Summary.Name, Type: m.Summary.Type, Capacity: uint64(m.Summary.Capacity), Free: uint64(m.Summary.FreeSpace)})
	}
	return out, nil
}

// Datastore is a storage of the host.
type Datastore struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Capacity uint64 `json:"capacity"`
	Free     uint64 `json:"free"`
}

// Networks lists the port groups VMs can connect to.
func (c *Client) Networks(ctx context.Context) ([]string, error) {
	ns, err := c.find.NetworkList(ctx, "*")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range ns {
		out = append(out, strings.TrimPrefix(n.GetInventoryPath(), c.dc.InventoryPath+"/network/"))
	}
	return out, nil
}

// Inventory is what the console shows about an ESXi host.
func (c *Client) Inventory(ctx context.Context, host string) (*pve.Inventory, error) {
	vms, err := c.VMs(ctx)
	if err != nil {
		return nil, err
	}
	dss, err := c.Datastores(ctx)
	if err != nil {
		return nil, err
	}
	inv := &pve.Inventory{Platform: "vmware", Node: host, Version: "ESXi " + c.Version}
	for _, v := range vms {
		status := "stopped"
		if v.Power == "poweredOn" {
			status = "running"
		}
		inv.Guests = append(inv.Guests, pve.Guest{VMID: v.VMID, ID: v.UUID, Name: v.Name, Type: "vm", Node: host,
			Status: status, MaxDisk: v.DiskSize(), MaxMem: uint64(v.MemoryMB) << 20, Template: v.Template})
	}
	for _, d := range dss {
		inv.Storage = append(inv.Storage, d.Name)
	}
	return inv, nil
}
