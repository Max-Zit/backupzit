package vmware

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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
// mounted), registers the VM in dir and starts it.
func (c *Client) InstantStart(ctx context.Context, ds, nfsHost, dir, name string, start bool) (string, []string, error) {
	if err := c.CheckSSH(ctx); err != nil {
		return "", nil, err
	}
	if !c.exists(ctx, "/vmfs/volumes/"+ds) {
		if _, err := c.run(ctx, "esxcli storage nfs add -H "+sq(nfsHost)+" -s /backupzit -v "+sq(ds)); err != nil {
			return "", nil, fmt.Errorf("mount the backup as datastore %s: %w", ds, err)
		}
	}
	vmx := "/vmfs/volumes/" + ds + "/" + dir + "/" + dir + ".vmx"
	out, err := c.run(ctx, "vim-cmd solo/registervm "+sq(vmx)+" "+sq(name))
	if err != nil {
		return "", nil, fmt.Errorf("register VM: %w", err)
	}
	moid := strings.TrimSpace(lastLines(out, 1))
	if _, err := strconv.Atoi(moid); err != nil {
		return "", nil, fmt.Errorf("register VM: unexpected answer %q", out)
	}
	var notes []string
	if start {
		if _, err := c.run(ctx, "vim-cmd vmsvc/power.on "+sq(moid)); err != nil {
			notes = append(notes, "the VM could not be started: "+err.Error())
		}
	}
	return moid, notes, nil
}

// powerOff stops a VM (no error if it is off).
func (c *Client) powerOff(ctx context.Context, moid string) {
	if out, _ := c.run(ctx, "vim-cmd vmsvc/power.getstate "+sq(moid)); strings.Contains(out, "Powered on") {
		c.run(ctx, "vim-cmd vmsvc/power.off "+sq(moid))
	}
}

// InstantFinish shuts the VM down, copies its disks and configuration from
// the NFS datastore to datastore target and registers it from there; it is
// started again if it was running.
func (c *Client) InstantFinish(ctx context.Context, ds, dir, moid, target string, log func(string, ...any)) (string, error) {
	if err := c.CheckSSH(ctx); err != nil {
		return "", err
	}
	if !c.exists(ctx, "/vmfs/volumes/"+target) {
		return "", fmt.Errorf("datastore %s not found", target)
	}
	state, _ := c.run(ctx, "vim-cmd vmsvc/power.getstate "+sq(moid))
	wasOn := strings.Contains(state, "Powered on")
	src := "/vmfs/volumes/" + ds + "/" + dir
	dstDir := dir
	for i := 2; c.exists(ctx, "/vmfs/volumes/"+target+"/"+dstDir); i++ {
		dstDir = fmt.Sprintf("%s_%d", dir, i)
	}
	dst := "/vmfs/volumes/" + target + "/" + dstDir
	// Shut down cleanly through VMware Tools if possible.
	if wasOn {
		log("shutting the VM down for the copy")
		if _, err := c.run(ctx, "vim-cmd vmsvc/power.shutdown "+sq(moid)); err == nil {
			for i := 0; i < 60; i++ {
				if out, _ := c.run(ctx, "vim-cmd vmsvc/power.getstate "+sq(moid)); strings.Contains(out, "Powered off") {
					break
				}
				time.Sleep(5 * time.Second)
			}
		}
		c.powerOff(ctx, moid)
	}
	if _, err := c.run(ctx, "mkdir "+sq(dst)); err != nil {
		return "", err
	}
	// On failure the VM keeps running from the backup as before.
	copied := false
	defer func() {
		if !copied {
			c.run(context.Background(), "rm -rf "+sq(dst))
			if wasOn {
				c.run(context.Background(), "vim-cmd vmsvc/power.on "+sq(moid))
			}
		}
	}()
	out, err := c.run(ctx, "ls "+sq(src))
	if err != nil {
		return "", err
	}
	for _, f := range strings.Fields(out) {
		switch {
		case strings.HasSuffix(f, "-flat.vmdk"), strings.HasSuffix(f, "-ctk.vmdk"), strings.HasSuffix(f, "-delta.vmdk"), strings.HasSuffix(f, "-sesparse.vmdk"),
			strings.HasSuffix(f, ".vswp"), strings.HasSuffix(f, ".log"), strings.HasSuffix(f, ".lck"):
		case strings.HasSuffix(f, ".vmdk"):
			log("copying disk", "file", f)
			if _, err := c.run(ctx, "vmkfstools -i "+sq(src+"/"+f)+" -d thin "+sq(dst+"/"+f)); err != nil {
				return "", fmt.Errorf("copy %s: %w", f, err)
			}
		case strings.HasSuffix(f, ".vmx"), strings.HasSuffix(f, ".nvram"), strings.HasSuffix(f, ".vmsd"), strings.HasSuffix(f, ".vmxf"):
			if _, err := c.run(ctx, "cp "+sq(src+"/"+f)+" "+sq(dst+"/")); err != nil {
				return "", err
			}
		}
	}
	name, _ := c.run(ctx, "vim-cmd vmsvc/get.summary "+sq(moid)+" | grep -m1 ' name = ' | cut -d'\"' -f2")
	name = strings.TrimSpace(name)
	if _, err := c.run(ctx, "vim-cmd vmsvc/unregister "+sq(moid)); err != nil {
		return "", fmt.Errorf("unregister the instant VM: %w", err)
	}
	copied = true
	args := sq(dst + "/" + dir + ".vmx")
	if name != "" {
		args += " " + sq(name)
	}
	out, err = c.run(ctx, "vim-cmd solo/registervm "+args)
	if err != nil {
		return "", fmt.Errorf("register VM on %s: %w", target, err)
	}
	newID := strings.TrimSpace(lastLines(out, 1))
	if wasOn {
		c.run(ctx, "vim-cmd vmsvc/power.on "+sq(newID))
	}
	return newID, nil
}

// InstantDiscard stops and unregisters the VM; its files are on the NFS
// export and go with it.
func (c *Client) InstantDiscard(ctx context.Context, moid string) error {
	if err := c.CheckSSH(ctx); err != nil {
		return err
	}
	c.powerOff(ctx, moid)
	if _, err := c.run(ctx, "vim-cmd vmsvc/unregister "+sq(moid)); err != nil && !strings.Contains(err.Error(), "not found") {
		return err
	}
	return nil
}

// InstantUnmount removes the NFS datastore.
func (c *Client) InstantUnmount(ctx context.Context, ds string) error {
	if !c.exists(ctx, "/vmfs/volumes/"+ds) {
		return nil
	}
	_, err := c.run(ctx, "esxcli storage nfs remove -v "+sq(ds))
	return err
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
