package vmware

import (
	"context"
	"strings"
	"testing"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/types"
)

func diskDev(key int32, backing types.BaseVirtualDeviceBackingInfo) *types.VirtualDisk {
	unit := int32(0)
	return &types.VirtualDisk{VirtualDevice: types.VirtualDevice{Key: key, ControllerKey: 1000, UnitNumber: &unit, Backing: backing}, CapacityInBytes: 1 << 30}
}

func flat(file string, parent *types.VirtualDiskFlatVer2BackingInfo) *types.VirtualDiskFlatVer2BackingInfo {
	return &types.VirtualDiskFlatVer2BackingInfo{VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{FileName: file}, Parent: parent, ThinProvisioned: types.NewBool(false)}
}

func TestDevicesDeltaChain(t *testing.T) {
	base := "[ds1] SRV-DC1/SRV-DC1.vmdk"
	se := &types.VirtualDiskSeSparseBackingInfo{VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{FileName: "[ds1] SRV-DC1/SRV-DC1-000003.vmdk"}, ChangeId: "52 a/9",
		Parent: &types.VirtualDiskSeSparseBackingInfo{VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{FileName: "[ds1] SRV-DC1/SRV-DC1-000002.vmdk"},
			Parent: &types.VirtualDiskSeSparseBackingInfo{VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{FileName: base}}}}
	devs := object.VirtualDeviceList{
		&types.VirtualLsiLogicSASController{VirtualSCSIController: types.VirtualSCSIController{VirtualController: types.VirtualController{VirtualDevice: types.VirtualDevice{Key: 1000}}}},
		diskDev(2000, flat(base, nil)),
		diskDev(2001, flat("[ds1] SRV-DC1/SRV-DC1_1-000002.vmdk", flat("[ds1] SRV-DC1/SRV-DC1_1.vmdk", nil))),
		diskDev(2002, se),
	}
	disks, _ := devices(devs)
	if len(disks) != 3 {
		t.Fatalf("disks: %+v", disks)
	}
	if d := disks[0]; d.Delta || d.Base != "" || d.Key != "scsi0:0" {
		t.Errorf("base disk: %+v", d)
	}
	if d := disks[1]; !d.Delta || d.Base != "[ds1] SRV-DC1/SRV-DC1_1.vmdk" {
		t.Errorf("flat delta: %+v", d)
	}
	if d := disks[2]; !d.Delta || d.Base != base || d.ChangeID != "52 a/9" || !d.Thin {
		t.Errorf("SEsparse delta: %+v", d)
	}
}

// #59: the disks of a quiesced snapshot may be a delta written by the
// quiescing on top of the VM's base disk. That delta is the backup's own and
// is read (through hot-add or a copy); a delta the VM had before the
// snapshot is still refused.
func TestPlanDiskOwnSnapshotDelta(t *testing.T) {
	c := &Client{}
	base := "[ds1] SRV-DC1/SRV-DC1.vmdk"
	before := VM{Name: "SRV-DC1", Power: "poweredOn", Disks: []Disk{{DevKey: 2000, Key: "scsi0:0", File: base, Capacity: 1 << 30, Thin: true}}}
	delta := Disk{DevKey: 2000, Key: "scsi0:0", File: "[ds1] SRV-DC1/SRV-DC1-000002.vmdk", Capacity: 1 << 30, Delta: true, Base: base}

	p, err := c.planDisk(context.Background(), before, &snapshot{}, delta, nil)
	if err != nil {
		t.Fatalf("own snapshot delta refused: %v", err)
	}
	if !p.chain || p.volume != base || !p.disk.Thin || p.disk.File != delta.File {
		t.Errorf("plan: chain=%v volume=%q thin=%v file=%q", p.chain, p.volume, p.disk.Thin, p.disk.File)
	}
	// Never read from the delta file directly (only hot-add or a copy).
	if _, err := c.storeDisk(context.Background(), nil, before, p, nil, nil); err == nil || !strings.Contains(err.Error(), "hot-add") {
		t.Errorf("chain read directly: %v", err)
	}

	refused := map[string]func() (VM, Disk){
		"VM had a delta before": func() (VM, Disk) {
			v := before
			v.Disks = []Disk{{DevKey: 2000, File: "[ds1] SRV-DC1/SRV-DC1-000001.vmdk", Delta: true, Base: base}}
			return v, delta
		},
		"chain ends at another disk": func() (VM, Disk) {
			d := delta
			d.Base = "[ds1] other/other.vmdk"
			return before, d
		},
		"chain not known": func() (VM, Disk) {
			d := delta
			d.Base = ""
			return before, d
		},
		"disk not in the VM before": func() (VM, Disk) {
			d := delta
			d.DevKey = 2001
			return before, d
		},
		"VM off": func() (VM, Disk) {
			v := before
			v.Power = "poweredOff"
			return v, delta
		},
	}
	for name, f := range refused {
		v, d := f()
		if _, err := c.planDisk(context.Background(), v, &snapshot{}, d, nil); err == nil || !strings.Contains(err.Error(), "snapshot delta disk") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A base disk is planned as before.
	p, err = c.planDisk(context.Background(), before, &snapshot{}, before.Disks[0], nil)
	if err != nil || p.chain || p.volume != base {
		t.Errorf("base disk: %+v %v", p, err)
	}
}
