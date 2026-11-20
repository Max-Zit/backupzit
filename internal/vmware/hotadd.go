package vmware

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/types"
)

// Hot-add (opt-in per VMware host): instead of copying each disk of a
// running VM's snapshot on the datastore, the snapshot's base disks are
// attached to the agent's own VM (a Linux agent in a VM that sees the
// datastore), read there through the block device - with changed block
// tracking only the changed areas - and detached again before the
// snapshot is removed. No temporary copy, no extra space on the datastore.
//
// The disks are attached independent-nonpersistent: the agent's VM opens
// them read-only and anything written goes to a redo log that vSphere
// discards when they are detached, so the VM's disk is never changed.
// Anything that fails falls back to the copy on the datastore.

// hotAdd is the agent's own VM, to which disks are attached.
type hotAdd struct {
	c     *Client
	proxy *object.VirtualMachine
	name  string
	dir   string // the proxy's own folder ("[ds] name"), whose disks are its own
	log   func(string, ...any)
}

// openHotAdd finds the agent's own VM and detaches what an interrupted
// earlier backup left attached to it.
func (c *Client) openHotAdd(ctx context.Context, log func(string, ...any)) (*hotAdd, error) {
	ids, err := ownVMUUIDs()
	if err != nil {
		return nil, err
	}
	si := object.NewSearchIndex(c.vim)
	for _, id := range ids {
		ref, err := si.FindByUuid(ctx, c.dc, id, true, nil)
		if err != nil || ref == nil {
			continue
		}
		vm, ok := ref.(*object.VirtualMachine)
		if !ok {
			continue
		}
		h := &hotAdd{c: c, proxy: vm, log: log}
		if h.name, err = vm.ObjectName(ctx); err != nil {
			return nil, err
		}
		devs, err := vm.Device(ctx)
		if err != nil {
			return nil, err
		}
		for _, d := range devs.SelectByType((*types.VirtualDisk)(nil)) {
			if b, ok := d.(*types.VirtualDisk).Backing.(*types.VirtualDiskFlatVer2BackingInfo); ok && b.DiskMode != string(types.VirtualDiskModeIndependent_nonpersistent) {
				h.dir = vmDir(b.FileName) // the proxy's own system disk
				break
			}
		}
		h.cleanup(ctx)
		return h, nil
	}
	return nil, errors.New("the agent's machine was not found as a VM of this host or vCenter (hot-add needs the proxy agent in a VM that sees the datastores)")
}

// vmDir returns the folder of a datastore path, "[ds] vm/disk.vmdk" -> "[ds] vm".
func vmDir(file string) string {
	if p, err := parseDSPath(file); err == nil {
		return "[" + p.DS + "] " + p.Dir()
	}
	return path.Dir(file)
}

// borrowed reports whether a disk of the proxy is one hot-add attached: an
// independent-nonpersistent disk of another VM's folder.
func (h *hotAdd) borrowed(d *types.VirtualDisk) (string, bool) {
	b, ok := d.Backing.(*types.VirtualDiskFlatVer2BackingInfo)
	if !ok || b.DiskMode != string(types.VirtualDiskModeIndependent_nonpersistent) {
		return "", false
	}
	return b.FileName, h.dir == "" || vmDir(b.FileName) != h.dir
}

// cleanup detaches disks of other VMs that an interrupted backup (agent
// stopped, VM reset) left attached to the proxy. Their files are kept.
func (h *hotAdd) cleanup(ctx context.Context) {
	devs, err := h.proxy.Device(ctx)
	if err != nil {
		return
	}
	for _, d := range devs.SelectByType((*types.VirtualDisk)(nil)) {
		if file, ok := h.borrowed(d.(*types.VirtualDisk)); ok {
			h.log("hot-add: detaching a disk left attached by an earlier backup", "disk", file)
			if err := h.detachFile(ctx, file); err != nil {
				h.log("hot-add: detaching failed", "disk", file, "error", err)
			}
		}
	}
}

// attach attaches file (a base disk of a snapshot) to the proxy and returns
// its block device, opened read-only, and a function that releases it.
func (h *hotAdd) attach(ctx context.Context, file string, capacity uint64) (io.ReaderAt, func(), error) {
	unlock, err := lockHotAdd()
	if err != nil {
		return nil, nil, err
	}
	defer unlock()
	before, err := blockDevices()
	if err != nil {
		return nil, nil, err
	}
	devs, err := h.proxy.Device(ctx)
	if err != nil {
		return nil, nil, err
	}
	ctrl, err := devs.FindSCSIController("")
	if err != nil {
		return nil, nil, fmt.Errorf("the agent's VM %s has no SCSI controller with a free slot: %w", h.name, err)
	}
	disk := devs.CreateDisk(ctrl, types.ManagedObjectReference{}, file)
	b := disk.Backing.(*types.VirtualDiskFlatVer2BackingInfo)
	b.DiskMode = string(types.VirtualDiskModeIndependent_nonpersistent)
	b.ThinProvisioned = nil
	// No file operation: the existing disk is attached, never created.
	spec := types.VirtualMachineConfigSpec{DeviceChange: []types.BaseVirtualDeviceConfigSpec{
		&types.VirtualDeviceConfigSpec{Operation: types.VirtualDeviceConfigSpecOperationAdd, Device: disk},
	}}
	t, err := h.proxy.Reconfigure(ctx, spec)
	if err := h.c.wait(ctx, t, err); err != nil {
		return nil, nil, fmt.Errorf("attach %s to the agent's VM %s: %w", file, h.name, err)
	}
	dev, err := waitForDisk(ctx, before, capacity)
	if err != nil {
		if derr := h.detachFile(context.WithoutCancel(ctx), file); derr != nil {
			err = fmt.Errorf("%w; detaching it again failed: %v", err, derr)
		}
		return nil, nil, err
	}
	f, err := os.Open(dev) // #nosec G304 -- the block device of the disk just attached
	if err != nil {
		_ = h.detachFile(context.WithoutCancel(ctx), file)
		return nil, nil, err
	}
	release := func() {
		_ = f.Close()
		ctx := context.WithoutCancel(ctx)
		unlock, err := lockHotAdd()
		if err == nil {
			defer unlock()
		}
		releaseDisk(dev, h.log)
		if err := h.detachFile(ctx, file); err != nil {
			h.log("hot-add: detaching failed; the next backup detaches it", "disk", file, "error", err)
		}
	}
	h.log("hot-add: disk attached to the agent's VM", "disk", file, "device", dev)
	return f, release, nil
}

// detachFile removes the proxy's disk device with the given file. The file
// itself is kept: it is the backed-up VM's disk.
func (h *hotAdd) detachFile(ctx context.Context, file string) error {
	devs, err := h.proxy.Device(ctx)
	if err != nil {
		return err
	}
	for _, d := range devs.SelectByType((*types.VirtualDisk)(nil)) {
		got, ok := h.borrowed(d.(*types.VirtualDisk))
		if !ok || !strings.EqualFold(got, file) {
			continue
		}
		spec := types.VirtualMachineConfigSpec{DeviceChange: []types.BaseVirtualDeviceConfigSpec{
			// No FileOperation "destroy": only the device goes.
			&types.VirtualDeviceConfigSpec{Operation: types.VirtualDeviceConfigSpecOperationRemove, Device: d},
		}}
		t, err := h.proxy.Reconfigure(ctx, spec)
		return h.c.wait(ctx, t, err)
	}
	return nil
}

// hotAddProxy returns the agent's VM for hot-add when the connection asks
// for it, or nil; the error says why hot-add cannot be used.
func (c *Client) hotAddProxy(ctx context.Context, log func(string, ...any)) (*hotAdd, error) {
	if !c.conn.HotAdd {
		return nil, nil
	}
	if !c.useAPI {
		return nil, errors.New("hot-add needs the vSphere API with write access (vCenter or a licensed host)")
	}
	h, err := c.openHotAdd(ctx, log)
	if err != nil {
		return nil, err
	}
	log("hot-add: disks of running VMs are read through the agent's VM", "vm", h.name)
	return h, nil
}

// MachineUUIDs are the BIOS UUID of the machine this agent runs on in both
// byte orders (Linux in a VM; nil elsewhere). The agent reports the first
// with its polls, so the console finds the proxy's VM in the inventory.
func MachineUUIDs() []string {
	ids, err := ownVMUUIDs()
	if err != nil {
		return nil
	}
	return ids
}

// SameMachine tells whether a machine UUID reported by an agent is a VM's
// BIOS UUID (config.uuid), in either byte order.
func SameMachine(machineUUID, biosUUID string) bool {
	m, b := strings.ToLower(machineUUID), strings.ToLower(biosUUID)
	return m != "" && b != "" && (m == b || swapUUID(m) == b)
}

// swapUUID reverses the byte order of the first three fields.
func swapUUID(id string) string {
	rev := func(s string) string {
		var b strings.Builder
		for i := len(s) - 2; i >= 0; i -= 2 {
			b.WriteString(s[i : i+2])
		}
		return b.String()
	}
	p := strings.Split(id, "-")
	if len(p) != 5 {
		return id
	}
	return rev(p[0]) + "-" + rev(p[1]) + "-" + rev(p[2]) + "-" + p[3] + "-" + p[4]
}
