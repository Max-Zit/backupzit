package vmware

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/max-zit/backupzit/internal/repo"
)

// Instant recovery on ESXi: the proxy agent serves the VM's disks from the
// backup over NFS (internal/instantnfs); the host mounts that as a
// datastore and runs the VM from it. Finishing copies the disks to a real
// datastore (the free ESXi license has no Storage vMotion, so the VM is
// shut down for the copy); discarding deletes the VM.

// InstantDisk is a disk of an instant VM: its flat file in the export and
// the image in the backup.
type InstantDisk struct {
	Flat  string `json:"flat"`
	Image int    `json:"image"`
	Key   string `json:"key"`
}

// InstantPlan is what InstantPrepare created.
type InstantPlan struct {
	Name  string
	Disks []InstantDisk
	Notes []string
}

// InstantPrepare writes the configuration, NVRAM and disk descriptors of VM
// vmid of snapshot sn into dir and creates the (sparse) flat files of the
// disks. asNew gives the VM a new identity (the original still exists).
func InstantPrepare(sn *repo.Snapshot, vmid int, dir, name string, asNew bool) (*InstantPlan, error) {
	var g *repo.Guest
	for i := range sn.Guests {
		if sn.Guests[i].Platform == "vmware" && sn.Guests[i].VMID == vmid {
			g = &sn.Guests[i]
		}
	}
	if g == nil {
		return nil, fmt.Errorf("VM %d is not in backup %s", vmid, sn.ID.Short())
	}
	var cfg guestConfig
	if err := json.Unmarshal([]byte(g.Config), &cfg); err != nil || cfg.VMX == "" {
		return nil, errors.New("the backup has no VM configuration")
	}
	if err := validName(name); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	base := filepath.Base(dir)
	plan := &InstantPlan{Name: name}
	v := parseVMX([]byte(cfg.VMX))
	restored := map[string]bool{}
	for i, d := range g.Disks {
		if d.Image < 0 || d.Image >= len(sn.Images) {
			return nil, fmt.Errorf("disk %s has no data in the backup", d.Key)
		}
		size := int64(sn.Images[d.Image].Size)
		file := base + ".vmdk"
		if i > 0 {
			file = fmt.Sprintf("%s_%d.vmdk", base, i)
		}
		flat := strings.TrimSuffix(file, ".vmdk") + "-flat.vmdk"
		if err := os.WriteFile(filepath.Join(dir, file), descriptor(flat, size, adapterFor(d.Key)), 0o600); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(filepath.Join(dir, flat), os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return nil, err
		}
		err = f.Truncate(size)
		f.Close()
		if err != nil {
			return nil, err
		}
		v.Set(d.Key+".fileName", file)
		restored[strings.ToLower(d.Key)] = true
		plan.Disks = append(plan.Disks, InstantDisk{Flat: flat, Image: d.Image, Key: d.Key})
	}
	for _, k := range v.Keys() {
		if strings.HasSuffix(strings.ToLower(k), ".filename") && strings.HasSuffix(strings.ToLower(v.Get(k)), ".vmdk") {
			if dev := k[:len(k)-len(".fileName")]; !restored[strings.ToLower(dev)] {
				v.Set(dev+".present", "FALSE")
				plan.Notes = append(plan.Notes, fmt.Sprintf("disk %s was not in the backup and was removed", dev))
			}
		}
	}
	plan.Notes = append(plan.Notes, adaptVMX(v, name, base, asNew)...)
	if len(cfg.NVRAM) > 0 {
		if err := os.WriteFile(filepath.Join(dir, base+".nvram"), cfg.NVRAM, 0o600); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, base+".vmx"), v.Bytes(), 0o600); err != nil {
		return nil, err
	}
	return plan, nil
}

// descriptor is the text descriptor of a flat disk.
func descriptor(flat string, size int64, adapter string) []byte {
	sectors := size / 512
	cyl := sectors / (255 * 63)
	if cyl < 1 {
		cyl = 1
	}
	return []byte(fmt.Sprintf(`# Disk DescriptorFile
version=1
encoding="UTF-8"
CID=fffffffe
parentCID=ffffffff
createType="vmfs"

# Extent description
RW %d VMFS "%s"

# The Disk Data Base
#DDB

ddb.adapterType = "%s"
ddb.geometry.cylinders = "%d"
ddb.geometry.heads = "255"
ddb.geometry.sectors = "63"
ddb.virtualHWVersion = "13"
`, sectors, flat, adapter, cyl))
}

// InstantDatastore is the name of the NFS datastore of a proxy agent.
func InstantDatastore(agentHost string) string {
	return "backupzit-" + strings.ToLower(badNameChars.ReplaceAllString(strings.Split(agentHost, ".")[0], "_"))
}

// InstantStart mounts the agent's NFS export as datastore ds (unless
// mounted), registers the VM in dir and starts it; on vCenter on the ESXi
// host node (default: the first connected host).
func (c *Client) InstantStart(ctx context.Context, ds, nfsHost, dir, name, node string, start bool) (string, []string, error) {
	if err := c.needSSH(ctx); err != nil {
		return "", nil, err
	}
	if !c.datastoreExists(ctx, ds) {
		if err := c.mountNFS(ctx, ds, nfsHost, node); err != nil {
			return "", nil, fmt.Errorf("mount the backup as datastore %s: %w", ds, err)
		}
	}
	moid, err := c.registerVM(ctx, dsPath{DS: ds, Path: dir + "/" + dir + ".vmx"}, name, node)
	if err != nil {
		return "", nil, fmt.Errorf("register VM: %w", err)
	}
	var notes []string
	if start {
		if err := c.powerOn(ctx, moid); err != nil {
			notes = append(notes, "the VM could not be started: "+err.Error())
		}
	}
	return moid, notes, nil
}

// InstantFinish shuts the VM down, copies its disks and configuration from
// the NFS datastore to datastore target and registers it from there; it is
// started again if it was running.
func (c *Client) InstantFinish(ctx context.Context, ds, dir, moid, target string, log func(string, ...any)) (string, error) {
	if err := c.needSSH(ctx); err != nil {
		return "", err
	}
	if !c.datastoreExists(ctx, target) {
		return "", fmt.Errorf("datastore %s not found", target)
	}
	if c.VCenter {
		// vCenter moves the running VM with Storage vMotion: no downtime.
		log("moving the VM to the datastore with Storage vMotion", "datastore", target)
		d, err := c.find.Datastore(ctx, target)
		if err != nil {
			return "", err
		}
		ref := d.Reference()
		vm := object.NewVirtualMachine(c.vim, vmRef(moid))
		t, err := vm.Relocate(ctx, types.VirtualMachineRelocateSpec{Datastore: &ref}, types.VirtualMachineMovePriorityHighPriority)
		if err := c.wait(ctx, t, err); err != nil {
			return "", fmt.Errorf("Storage vMotion to %s: %w", target, err)
		}
		// Storage vMotion leaves the output files of serial and parallel
		// ports where they were; point them into the VM's new folder so
		// nothing uses the backup's datastore any more.
		if err := c.movePortFiles(ctx, vm, ds); err != nil {
			log("serial or parallel port files could not be moved", "err", err)
		}
		return moid, nil
	}
	wasOn := c.powerState(ctx, moid) == "poweredOn"
	dstDir := dir
	for i := 2; c.exists(ctx, target, dstDir); i++ {
		dstDir = fmt.Sprintf("%s_%d", dir, i)
	}
	// Shut down cleanly through VMware Tools if possible.
	if wasOn {
		log("shutting the VM down for the copy")
		c.shutdown(ctx, moid)
	}
	if err := c.mkdir(ctx, target, dstDir); err != nil {
		return "", err
	}
	// On failure the VM keeps running from the backup as before.
	copied := false
	defer func() {
		if !copied {
			c.removeDir(context.Background(), target, dstDir)
			if wasOn {
				c.powerOn(context.Background(), moid)
			}
		}
	}()
	files, err := c.listDir(ctx, ds, dir)
	if err != nil {
		return "", err
	}
	for _, f := range files {
		src, dst := dsPath{DS: ds, Path: dir + "/" + f}, dsPath{DS: target, Path: dstDir + "/" + f}
		switch {
		case strings.HasSuffix(f, "-flat.vmdk"), strings.HasSuffix(f, "-ctk.vmdk"), strings.HasSuffix(f, "-delta.vmdk"), strings.HasSuffix(f, "-sesparse.vmdk"),
			strings.HasSuffix(f, ".vswp"), strings.HasSuffix(f, ".log"), strings.HasSuffix(f, ".lck"):
		case strings.HasSuffix(f, ".vmdk"):
			log("copying disk", "file", f)
			if err := c.copyDisk(ctx, src, dst); err != nil {
				return "", fmt.Errorf("copy %s: %w", f, err)
			}
		case strings.HasSuffix(f, ".vmx"), strings.HasSuffix(f, ".nvram"), strings.HasSuffix(f, ".vmsd"), strings.HasSuffix(f, ".vmxf"):
			if err := c.copyFile(ctx, src, dst); err != nil {
				return "", err
			}
		}
	}
	name := c.vmName(ctx, moid)
	if err := c.unregisterVM(ctx, moid); err != nil {
		return "", fmt.Errorf("unregister the instant VM: %w", err)
	}
	copied = true
	newID, err := c.registerVM(ctx, dsPath{DS: target, Path: dstDir + "/" + dir + ".vmx"}, name, "")
	if err != nil {
		return "", fmt.Errorf("register VM on %s: %w", target, err)
	}
	if wasOn {
		c.powerOn(ctx, newID)
	}
	return newID, nil
}

// movePortFiles points file-backed serial and parallel ports that write to
// datastore ds into the folder of the VM's configuration file.
func (c *Client) movePortFiles(ctx context.Context, vm *object.VirtualMachine, ds string) error {
	var m mo.VirtualMachine
	if err := vm.Properties(ctx, vm.Reference(), []string{"config.hardware.device", "config.files.vmPathName"}, &m); err != nil {
		return err
	}
	vmx, err := parseDSPath(m.Config.Files.VmPathName)
	if err != nil {
		return err
	}
	var changes []types.BaseVirtualDeviceConfigSpec
	fix := func(dev types.BaseVirtualDevice, b *types.VirtualDeviceFileBackingInfo) {
		p, err := parseDSPath(b.FileName)
		if err != nil || p.DS != ds {
			return
		}
		b.FileName, b.Datastore = vmx.join(path.Base(p.Path)).String(), nil
		changes = append(changes, &types.VirtualDeviceConfigSpec{Operation: types.VirtualDeviceConfigSpecOperationEdit, Device: dev})
	}
	for _, d := range m.Config.Hardware.Device {
		switch x := d.(type) {
		case *types.VirtualSerialPort:
			if b, ok := x.Backing.(*types.VirtualSerialPortFileBackingInfo); ok {
				fix(x, &b.VirtualDeviceFileBackingInfo)
			}
		case *types.VirtualParallelPort:
			if b, ok := x.Backing.(*types.VirtualParallelPortFileBackingInfo); ok {
				fix(x, &b.VirtualDeviceFileBackingInfo)
			}
		}
	}
	if len(changes) == 0 {
		return nil
	}
	t, err := vm.Reconfigure(ctx, types.VirtualMachineConfigSpec{DeviceChange: changes})
	return c.wait(ctx, t, err)
}

// InstantDiscard stops and unregisters the VM; its files are on the NFS
// export and go with it.
func (c *Client) InstantDiscard(ctx context.Context, moid string) error {
	if err := c.needSSH(ctx); err != nil {
		return err
	}
	c.powerOff(ctx, moid)
	if err := c.unregisterVM(ctx, moid); err != nil && !strings.Contains(err.Error(), "not found") && !strings.Contains(err.Error(), "ManagedObjectNotFound") {
		return err
	}
	return nil
}

// InstantUnmount removes the NFS datastore.
func (c *Client) InstantUnmount(ctx context.Context, ds string) error {
	if !c.datastoreExists(ctx, ds) {
		return nil
	}
	return c.unmountNFS(ctx, ds)
}

// VMExists reports whether a VM with this instance UUID is registered.
func (c *Client) VMExists(ctx context.Context, uuid string) bool {
	vms, err := c.VMs(ctx)
	if err != nil {
		return false
	}
	for _, v := range vms {
		if strings.EqualFold(v.UUID, uuid) {
			return true
		}
	}
	return false
}
