package vmware

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// Operations that change the host. Licensed hosts (and vCenter) do them
// through the vSphere API; hosts with the free license, whose API is
// read-only, through SSH. Conn.Method can force one way.

// Methods of Conn.Method.
const (
	MethodAuto = ""
	MethodSSH  = "ssh"
	MethodAPI  = "api"
)

// Method reports how the host is changed: "api" or "ssh".
func (c *Client) Method() string {
	if c.useAPI {
		return MethodAPI
	}
	return MethodSSH
}

// chooseMethod decides between the API and SSH after connecting.
func (c *Client) chooseMethod() error {
	if c.VCenter {
		if c.conn.Method == MethodSSH {
			return errors.New("vCenter is changed through the vSphere API only; choose Automatic")
		}
		c.useAPI = true
		return nil
	}
	switch c.conn.Method {
	case MethodSSH:
		c.useAPI = false
	case MethodAPI:
		if !c.APIWrites {
			return errors.New("the host has the free ESXi license, whose API is read-only; use SSH (Automatic) for it")
		}
		c.useAPI = true
	default:
		c.useAPI = c.APIWrites
	}
	return nil
}

// needSSH checks SSH before operations that use it.
func (c *Client) needSSH(ctx context.Context) error {
	if c.useAPI {
		return nil
	}
	return c.CheckSSH(ctx)
}

func vmRef(moid string) types.ManagedObjectReference {
	return types.ManagedObjectReference{Type: "VirtualMachine", Value: moid}
}

func (c *Client) wait(ctx context.Context, t *object.Task, err error) error {
	if err != nil {
		return err
	}
	return t.Wait(ctx)
}

// mkdir creates a directory on a datastore.
func (c *Client) mkdir(ctx context.Context, ds, dir string) error {
	if c.useAPI {
		err := object.NewFileManager(c.vim).MakeDirectory(ctx, dsPath{DS: ds, Path: dir}.String(), c.dc, true)
		if err != nil && strings.Contains(err.Error(), "already exists") {
			return nil
		}
		return err
	}
	_, err := c.run(ctx, "mkdir -p "+sq("/vmfs/volumes/"+ds+"/"+dir))
	return err
}

// removeDir deletes a directory with its contents.
func (c *Client) removeDir(ctx context.Context, ds, dir string) error {
	if c.useAPI {
		t, err := object.NewFileManager(c.vim).DeleteDatastoreFile(ctx, dsPath{DS: ds, Path: dir}.String(), c.dc)
		return c.wait(ctx, t, err)
	}
	_, err := c.run(ctx, "rm -rf "+sq("/vmfs/volumes/"+ds+"/"+dir))
	return err
}

// removeDirIfEmpty deletes an empty directory (others may still use it).
func (c *Client) removeDirIfEmpty(ctx context.Context, ds, dir string) {
	if c.useAPI {
		if files, err := c.listDir(ctx, ds, dir); err == nil && len(files) == 0 {
			c.removeDir(ctx, ds, dir)
		}
		return
	}
	c.run(ctx, "rmdir "+sq("/vmfs/volumes/"+ds+"/"+dir)+" 2>/dev/null; true")
}

// exists reports whether a file or directory exists on a datastore.
func (c *Client) exists(ctx context.Context, ds, p string) bool {
	if c.useAPI {
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.fileURL(dsPath{DS: ds, Path: p}), nil)
		if err != nil {
			return false
		}
		found := false
		c.vim.Do(ctx, req, func(resp *http.Response) error {
			found = resp.StatusCode == http.StatusOK
			return nil
		})
		return found
	}
	_, err := c.run(ctx, "test -e "+sq("/vmfs/volumes/"+ds+"/"+p))
	return err == nil
}

// datastoreExists reports whether the host has a datastore of this name.
func (c *Client) datastoreExists(ctx context.Context, ds string) bool {
	if c.useAPI {
		dss, err := c.Datastores(ctx)
		if err != nil {
			return false
		}
		for _, d := range dss {
			if d.Name == ds {
				return true
			}
		}
		return false
	}
	_, err := c.run(ctx, "test -e "+sq("/vmfs/volumes/"+ds))
	return err == nil
}

// listDir lists the file names in a datastore directory.
func (c *Client) listDir(ctx context.Context, ds, dir string) ([]string, error) {
	if c.useAPI {
		d, err := c.find.Datastore(ctx, ds)
		if err != nil {
			return nil, err
		}
		b, err := d.Browser(ctx)
		if err != nil {
			return nil, err
		}
		t, err := b.SearchDatastore(ctx, dsPath{DS: ds, Path: dir}.String(), &types.HostDatastoreBrowserSearchSpec{
			Details: &types.FileQueryFlags{}, MatchPattern: []string{"*"}})
		if err != nil {
			return nil, err
		}
		info, err := t.WaitForResult(ctx)
		if err != nil {
			return nil, err
		}
		res, ok := info.Result.(types.HostDatastoreBrowserSearchResults)
		if !ok {
			return nil, nil
		}
		var out []string
		for _, f := range res.File {
			out = append(out, f.GetFileInfo().Path)
		}
		return out, nil
	}
	out, err := c.run(ctx, "ls "+sq("/vmfs/volumes/"+ds+"/"+dir))
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// copyFile copies a (small) datastore file.
func (c *Client) copyFile(ctx context.Context, src, dst dsPath) error {
	if c.useAPI {
		t, err := object.NewFileManager(c.vim).CopyDatastoreFile(ctx, src.String(), c.dc, dst.String(), c.dc, false)
		return c.wait(ctx, t, err)
	}
	_, err := c.run(ctx, "cp "+sq(src.local())+" "+sq(dst.local()))
	return err
}

// thinSpec describes a thin disk; adapter is the descriptor's adapter type
// ("ide", otherwise SCSI), which the API spells differently.
func thinSpec(adapter string) *types.FileBackedVirtualDiskSpec {
	a := types.VirtualDiskAdapterTypeLsiLogic
	if adapter == "ide" {
		a = types.VirtualDiskAdapterTypeIde
	}
	return &types.FileBackedVirtualDiskSpec{VirtualDiskSpec: types.VirtualDiskSpec{
		DiskType: string(types.VirtualDiskTypeThin), AdapterType: string(a)}}
}

// copyDisk copies a virtual disk into a new thin disk.
func (c *Client) copyDisk(ctx context.Context, src, dst dsPath) error {
	if c.useAPI {
		t, err := object.NewVirtualDiskManager(c.vim).CopyVirtualDisk(ctx, src.String(), c.dc, dst.String(), c.dc, thinSpec(""), false)
		return c.wait(ctx, t, err)
	}
	_, err := c.run(ctx, "vmkfstools -i "+sq(src.local())+" -d thin "+sq(dst.local()))
	return err
}

// deleteDisk removes a virtual disk (descriptor and data).
func (c *Client) deleteDisk(ctx context.Context, p dsPath) error {
	if c.useAPI {
		t, err := object.NewVirtualDiskManager(c.vim).DeleteVirtualDisk(ctx, p.String(), c.dc)
		return c.wait(ctx, t, err)
	}
	_, err := c.run(ctx, "vmkfstools -U "+sq(p.local()))
	return err
}

// moveDisk renames a virtual disk.
func (c *Client) moveDisk(ctx context.Context, src, dst dsPath) error {
	if c.useAPI {
		t, err := object.NewVirtualDiskManager(c.vim).MoveVirtualDisk(ctx, src.String(), c.dc, dst.String(), c.dc, false)
		return c.wait(ctx, t, err)
	}
	_, err := c.run(ctx, "vmkfstools -E "+sq(src.local())+" "+sq(dst.local()))
	return err
}

// hostAndPool are the host and the resource pool new VMs are placed in:
// the ESXi host itself, or on vCenter the host named node (default: the
// first connected host).
func (c *Client) hostAndPool(ctx context.Context, node string) (*object.HostSystem, *object.ResourcePool, error) {
	var h *object.HostSystem
	if !c.VCenter {
		var err error
		if h, err = c.find.DefaultHostSystem(ctx); err != nil {
			return nil, nil, err
		}
	} else {
		hs, err := c.Hosts(ctx)
		if err != nil {
			return nil, nil, err
		}
		for _, x := range hs {
			if x.Connected && (node == "" || strings.EqualFold(x.Name, node)) {
				h = object.NewHostSystem(c.vim, x.Ref)
				break
			}
		}
		if h == nil {
			if node != "" {
				return nil, nil, fmt.Errorf("ESXi host %s is not connected to vCenter", node)
			}
			return nil, nil, errors.New("vCenter has no connected ESXi host")
		}
	}
	p, err := h.ResourcePool(ctx)
	return h, p, err
}

// registerVM adds the VM of a .vmx file to the inventory and returns its ID.
func (c *Client) registerVM(ctx context.Context, vmx dsPath, name, node string) (string, error) {
	if c.useAPI {
		folders, err := c.dc.Folders(ctx)
		if err != nil {
			return "", err
		}
		h, pool, err := c.hostAndPool(ctx, node)
		if err != nil {
			return "", err
		}
		t, err := folders.VmFolder.RegisterVM(ctx, vmx.String(), name, false, pool, h)
		if err != nil {
			return "", err
		}
		info, err := t.WaitForResult(ctx)
		if err != nil {
			return "", err
		}
		ref, ok := info.Result.(types.ManagedObjectReference)
		if !ok {
			return "", errors.New("register VM: no VM in the answer")
		}
		return ref.Value, nil
	}
	args := sq(vmx.local())
	if name != "" {
		args += " " + sq(name)
	}
	out, err := c.run(ctx, "vim-cmd solo/registervm "+args)
	if err != nil {
		return "", err
	}
	moid := strings.TrimSpace(lastLines(out, 1))
	if _, err := strconv.Atoi(moid); err != nil {
		return "", fmt.Errorf("unexpected answer %q", out)
	}
	return moid, nil
}

// unregisterVM removes a VM from the inventory; its files stay.
func (c *Client) unregisterVM(ctx context.Context, moid string) error {
	if c.useAPI {
		return object.NewVirtualMachine(c.vim, vmRef(moid)).Unregister(ctx)
	}
	_, err := c.run(ctx, "vim-cmd vmsvc/unregister "+sq(moid))
	return err
}

// powerState is poweredOn, poweredOff or suspended.
func (c *Client) powerState(ctx context.Context, moid string) string {
	if c.useAPI {
		var m mo.VirtualMachine
		if err := property.DefaultCollector(c.vim).RetrieveOne(ctx, vmRef(moid), []string{"runtime.powerState"}, &m); err != nil {
			return ""
		}
		return string(m.Runtime.PowerState)
	}
	out, _ := c.run(ctx, "vim-cmd vmsvc/power.getstate "+sq(moid))
	switch {
	case strings.Contains(out, "Powered on"):
		return "poweredOn"
	case strings.Contains(out, "Suspended"):
		return "suspended"
	case strings.Contains(out, "Powered off"):
		return "poweredOff"
	}
	return ""
}

// powerOn starts a VM.
func (c *Client) powerOn(ctx context.Context, moid string) error {
	if c.useAPI {
		t, err := object.NewVirtualMachine(c.vim, vmRef(moid)).PowerOn(ctx)
		return c.wait(ctx, t, err)
	}
	_, err := c.run(ctx, "vim-cmd vmsvc/power.on "+sq(moid))
	return err
}

// powerOff stops a VM hard (no error if it is off).
func (c *Client) powerOff(ctx context.Context, moid string) error {
	if c.powerState(ctx, moid) != "poweredOn" {
		return nil
	}
	if c.useAPI {
		t, err := object.NewVirtualMachine(c.vim, vmRef(moid)).PowerOff(ctx)
		return c.wait(ctx, t, err)
	}
	_, err := c.run(ctx, "vim-cmd vmsvc/power.off "+sq(moid))
	return err
}

// shutdown asks VMware Tools to shut the guest down and waits up to five
// minutes; then the VM is powered off.
func (c *Client) shutdown(ctx context.Context, moid string) {
	var err error
	if c.useAPI {
		err = object.NewVirtualMachine(c.vim, vmRef(moid)).ShutdownGuest(ctx)
	} else {
		_, err = c.run(ctx, "vim-cmd vmsvc/power.shutdown "+sq(moid))
	}
	if err == nil {
		for i := 0; i < 60 && c.powerState(ctx, moid) == "poweredOn"; i++ {
			time.Sleep(5 * time.Second)
		}
	}
	c.powerOff(ctx, moid)
}

// removeAllSnapshots deletes all snapshots of a VM.
func (c *Client) removeAllSnapshots(ctx context.Context, moid string) error {
	if c.useAPI {
		t, err := object.NewVirtualMachine(c.vim, vmRef(moid)).RemoveAllSnapshot(ctx, types.NewBool(true))
		return c.wait(ctx, t, err)
	}
	_, err := c.run(ctx, "vim-cmd vmsvc/snapshot.removeall "+sq(moid))
	return err
}

// reloadVM rereads a VM's configuration file.
func (c *Client) reloadVM(ctx context.Context, moid string) error {
	if c.useAPI {
		_, err := methods.Reload(ctx, c.vim, &types.Reload{This: vmRef(moid)})
		return err
	}
	_, err := c.run(ctx, "vim-cmd vmsvc/reload "+sq(moid))
	return err
}

// vmName is the inventory name of a VM.
func (c *Client) vmName(ctx context.Context, moid string) string {
	if c.useAPI {
		var m mo.VirtualMachine
		if err := property.DefaultCollector(c.vim).RetrieveOne(ctx, vmRef(moid), []string{"name"}, &m); err != nil {
			return ""
		}
		return m.Name
	}
	out, _ := c.run(ctx, "vim-cmd vmsvc/get.summary "+sq(moid)+" | grep -m1 ' name = ' | cut -d'\"' -f2")
	return strings.TrimSpace(out)
}

// mountNFS mounts an NFS export (path /backupzit) of nfsHost as datastore ds
// on the host (node on vCenter).
func (c *Client) mountNFS(ctx context.Context, ds, nfsHost, node string) error {
	if c.useAPI {
		h, _, err := c.hostAndPool(ctx, node)
		if err != nil {
			return err
		}
		dss, err := h.ConfigManager().DatastoreSystem(ctx)
		if err != nil {
			return err
		}
		_, err = dss.CreateNasDatastore(ctx, types.HostNasVolumeSpec{RemoteHost: nfsHost, RemotePath: "/backupzit",
			LocalPath: ds, AccessMode: string(types.HostMountModeReadWrite), Type: string(types.HostFileSystemVolumeFileSystemTypeNFS)})
		return err
	}
	_, err := c.run(ctx, "esxcli storage nfs add -H "+sq(nfsHost)+" -s /backupzit -v "+sq(ds))
	return err
}

// unmountNFS removes the NFS datastore ds (from every host that has it).
func (c *Client) unmountNFS(ctx context.Context, ds string) error {
	if c.useAPI {
		d, err := c.find.Datastore(ctx, ds)
		if err != nil {
			return nil // not mounted
		}
		hosts, err := d.AttachedHosts(ctx)
		if err != nil {
			return err
		}
		for _, h := range hosts {
			dss, err := h.ConfigManager().DatastoreSystem(ctx)
			if err != nil {
				return err
			}
			if err := dss.Remove(ctx, d); err != nil {
				return err
			}
		}
		return nil
	}
	_, err := c.run(ctx, "esxcli storage nfs remove -v "+sq(ds))
	return err
}

// createDiskWithData creates the disk p of the image through the API: the
// image is uploaded as a sparse VMDK next to it and converted into p.
func (c *Client) uploadDisk(ctx context.Context, upload func(dsPath) error, p dsPath, adapter string) error {
	tmp := p.join(strings.TrimSuffix(path.Base(p.Path), ".vmdk") + "-bzupload.vmdk")
	fm := object.NewFileManager(c.vim)
	if t, err := fm.DeleteDatastoreFile(ctx, tmp.String(), c.dc); err == nil {
		t.Wait(ctx)
	}
	if err := upload(tmp); err != nil {
		return err
	}
	defer func() {
		if t, err := fm.DeleteDatastoreFile(context.Background(), tmp.String(), c.dc); err == nil {
			t.Wait(context.Background())
		}
	}()
	t, err := object.NewVirtualDiskManager(c.vim).CopyVirtualDisk(ctx, tmp.String(), c.dc, p.String(), c.dc, thinSpec(adapter), false)
	if err := c.wait(ctx, t, err); err != nil {
		return fmt.Errorf("convert the uploaded disk: %w", err)
	}
	return nil
}
