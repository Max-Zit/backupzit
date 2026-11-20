package vmware

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/max-zit/backupzit/internal/diskimg"
	"github.com/max-zit/backupzit/internal/leftover"
)

const snapPrefix = "backupzit_"

// snapshot is a snapshot taken for a backup.
type snapshot struct {
	Ref types.ManagedObjectReference
	ID  int32 // vim-cmd snapshot id
	// Name and Created: set by leftovers.
	Name    string
	Created time.Time
	// Quiesced is true when VMware Tools prepared the guest.
	Quiesced bool
}

// findSnapshot looks up a snapshot of vm by name.
func (c *Client) findSnapshot(ctx context.Context, vm types.ManagedObjectReference, name string) (*snapshot, error) {
	var m mo.VirtualMachine
	if err := property.DefaultCollector(c.vim).RetrieveOne(ctx, vm, []string{"snapshot"}, &m); err != nil {
		return nil, err
	}
	if m.Snapshot == nil {
		return nil, nil
	}
	var walk func([]types.VirtualMachineSnapshotTree) *snapshot
	walk = func(ts []types.VirtualMachineSnapshotTree) *snapshot {
		for _, t := range ts {
			if t.Name == name {
				return &snapshot{Ref: t.Snapshot, ID: t.Id, Quiesced: t.Quiesced}
			}
			if s := walk(t.ChildSnapshotList); s != nil {
				return s
			}
		}
		return nil
	}
	return walk(m.Snapshot.RootSnapshotList), nil
}

// leftovers lists BackupZit snapshots left behind by an interrupted backup.
func (c *Client) leftovers(ctx context.Context, vm types.ManagedObjectReference) ([]snapshot, error) {
	var m mo.VirtualMachine
	if err := property.DefaultCollector(c.vim).RetrieveOne(ctx, vm, []string{"snapshot"}, &m); err != nil {
		return nil, err
	}
	var out []snapshot
	var walk func([]types.VirtualMachineSnapshotTree)
	walk = func(ts []types.VirtualMachineSnapshotTree) {
		for _, t := range ts {
			if strings.HasPrefix(t.Name, snapPrefix) {
				out = append(out, snapshot{Ref: t.Snapshot, ID: t.Id, Name: t.Name, Created: t.CreateTime})
			}
			walk(t.ChildSnapshotList)
		}
	}
	if m.Snapshot != nil {
		walk(m.Snapshot.RootSnapshotList)
	}
	return out, nil
}

// snapshotGrace bounds how long a cancelled backup still waits for its
// snapshot being created: the host finishes the task anyway, and a snapshot
// that appears after the run gave up would stay (S-1).
var snapshotGrace = 5 * time.Minute

// createSnapshot takes a snapshot without memory; quiesce asks VMware Tools
// to flush the guest's file systems and applications first. A cancel does
// not cut the creation short (up to snapshotGrace): the caller removes a
// snapshot finished after the cancel.
func (c *Client) createSnapshot(ctx context.Context, vm VM, name string, quiesce bool) (*snapshot, error) {
	run, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	stop := context.AfterFunc(ctx, func() { time.AfterFunc(snapshotGrace, cancel) })
	defer stop()
	if c.useAPI {
		task, err := object.NewVirtualMachine(c.vim, vm.Ref).CreateSnapshot(run, name, "BackupZit backup in progress", false, quiesce)
		if err != nil {
			return nil, err
		}
		if err := task.Wait(run); err != nil {
			return nil, err
		}
	} else {
		q := "0"
		if quiesce {
			q = "1"
		}
		if _, err := c.run(run, fmt.Sprintf("vim-cmd vmsvc/snapshot.create %s %s %s 0 %s", sq(vm.MoID), sq(name), sq("BackupZit backup in progress"), q)); err != nil {
			return nil, err
		}
	}
	s, err := c.findSnapshot(run, vm.Ref, name)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, errors.New("the snapshot was not created")
	}
	s.Quiesced = s.Quiesced || quiesce
	return s, nil
}

// removeSnapshot deletes a snapshot and merges its changes into the disks.
func (c *Client) removeSnapshot(ctx context.Context, vm VM, s snapshot) error {
	if c.useAPI {
		res, err := methods.RemoveSnapshot_Task(ctx, c.vim, &types.RemoveSnapshot_Task{This: s.Ref, RemoveChildren: false, Consolidate: types.NewBool(true)})
		if err != nil {
			return err
		}
		return object.NewTask(c.vim, res.Returnval).Wait(ctx)
	}
	_, err := c.run(ctx, fmt.Sprintf("vim-cmd vmsvc/snapshot.remove %s %d", sq(vm.MoID), s.ID))
	return err
}

// snapshotItem is the ledger record of a backup snapshot of vm.
func (c *Client) snapshotItem(vm VM, name string) leftover.Item {
	return leftover.Item{Platform: leftover.VMware, Kind: leftover.KindSnapshot, Host: c.conn.Host, VM: vm.UUID, VMName: vm.Name, Name: name}
}

// removeSnapshotVerified removes the snapshot called name and lists the
// VM's snapshots again to see that it is gone, up to len(waits)+1 times. A
// VM whose disks still need consolidation afterwards is consolidated. The
// ledger item goes once the snapshot is gone; otherwise it records the
// failure for the janitor and the console (S-1).
func (c *Client) removeSnapshotVerified(ctx context.Context, vm VM, name string, waits []time.Duration) (note string, err error) {
	it := c.snapshotItem(vm, name)
	err = leftover.Retry(ctx, waits, func(ctx context.Context) error {
		s, err := c.findSnapshot(ctx, vm.Ref, name)
		if err != nil || s == nil {
			return err
		}
		return c.removeSnapshot(ctx, vm, *s)
	}, func(ctx context.Context) (bool, error) {
		s, err := c.findSnapshot(ctx, vm.Ref, name)
		return err == nil && s == nil, err
	})
	if err != nil {
		leftover.Failed(it, err)
		return "", err
	}
	leftover.Done(it)
	return c.consolidate(ctx, vm), nil
}

// consolidate merges what a snapshot removal left in delta disks
// (runtime.consolidationNeeded, e.g. after a host or datastore hiccup).
// It returns a note when the VM still needs it.
func (c *Client) consolidate(ctx context.Context, vm VM) string {
	needed := func() bool {
		var m mo.VirtualMachine
		if err := property.DefaultCollector(c.vim).RetrieveOne(ctx, vm.Ref, []string{"runtime.consolidationNeeded"}, &m); err != nil {
			return false
		}
		return m.Runtime.ConsolidationNeeded
	}
	if !needed() {
		return ""
	}
	if !c.useAPI {
		return "the disks of VM " + vm.Name + " need consolidation after the snapshot removal; consolidate them in the host client (Snapshots → Consolidate)"
	}
	res, err := methods.ConsolidateVMDisks_Task(ctx, c.vim, &types.ConsolidateVMDisks_Task{This: vm.Ref})
	if err == nil {
		err = object.NewTask(c.vim, res.Returnval).Wait(ctx)
	}
	if err != nil || needed() {
		why := "it is still needed"
		if err != nil {
			why = err.Error()
		}
		return "the disks of VM " + vm.Name + " need consolidation after the snapshot removal and consolidating them failed (" + why + "); consolidate them in the vSphere Client (Snapshots → Consolidate)"
	}
	return "the disks of VM " + vm.Name + " were consolidated after the snapshot removal"
}

// snapshotDisks returns the disks as they are in a snapshot.
func (c *Client) snapshotDisks(ctx context.Context, s snapshot) ([]Disk, error) {
	var m mo.VirtualMachineSnapshot
	if err := property.DefaultCollector(c.vim).RetrieveOne(ctx, s.Ref, []string{"config.hardware.device"}, &m); err != nil {
		return nil, err
	}
	disks, _ := devices(m.Config.Hardware.Device)
	return disks, nil
}

// enableCBT switches on changed block tracking. With API access it takes
// effect at the backup's snapshot; on free hosts it can only be changed
// while the VM is off. It returns a note when CBT stays off.
func (c *Client) enableCBT(ctx context.Context, vm VM) (string, error) {
	if vm.CBT {
		return "", nil
	}
	if c.useAPI {
		task, err := object.NewVirtualMachine(c.vim, vm.Ref).Reconfigure(ctx, types.VirtualMachineConfigSpec{ChangeTrackingEnabled: types.NewBool(true)})
		if err != nil {
			return "", err
		}
		return "", task.Wait(ctx)
	}
	if vm.Power != "poweredOff" {
		return "changed block tracking is off and can be enabled on this host (free license) only while the VM is off; until then every backup reads the whole disks", nil
	}
	vmx, err := parseDSPath(vm.VMX)
	if err != nil {
		return "", err
	}
	cfg, err := c.get(ctx, vmx)
	if err != nil {
		return "", err
	}
	v := parseVMX(cfg)
	v.Set("ctkEnabled", "TRUE")
	for _, d := range vm.Disks {
		if d.Key != "" {
			v.Set(d.Key+".ctkEnabled", "TRUE")
		}
	}
	if err := c.put(ctx, vmx, v.Bytes()); err != nil {
		return "", err
	}
	if _, err := c.run(ctx, "vim-cmd vmsvc/reload "+sq(vm.MoID)); err != nil {
		return "", err
	}
	return "", nil
}

// changedAreas asks changed block tracking which parts of a disk differ
// from changeID ("*" for all allocated areas).
func (c *Client) changedAreas(ctx context.Context, vm VM, s snapshot, d Disk, changeID string) ([]diskimg.Area, error) {
	var areas []diskimg.Area
	var off int64
	for off < int64(d.Capacity) { // #nosec G115 -- disk and datastore sizes from vSphere are within range
		res, err := methods.QueryChangedDiskAreas(ctx, c.vim, &types.QueryChangedDiskAreas{
			This: vm.Ref, Snapshot: &s.Ref, DeviceKey: d.DevKey, StartOffset: off, ChangeId: changeID})
		if err != nil {
			return nil, err
		}
		for _, a := range res.Returnval.ChangedArea {
			areas = append(areas, diskimg.Area{Offset: uint64(a.Start), Length: uint64(a.Length)}) // #nosec G115 -- disk and datastore sizes from vSphere are within range
		}
		next := res.Returnval.StartOffset + res.Returnval.Length
		if next <= off {
			return nil, errors.New("changed block tracking returned no progress")
		}
		off = next
	}
	return areas, nil
}
