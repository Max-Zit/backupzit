package vmware

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/diskimg"
	"github.com/max-zit/backupzit/internal/repo"
)

// RestoreOptions choose where and how a VM is restored.
type RestoreOptions struct {
	VMID int // VM in the backup
	// AsNew creates a second VM next to the original (new name, UUID and
	// MAC addresses). Otherwise the original is restored in place; an
	// existing VM is only replaced with Overwrite.
	AsNew     bool
	Name      string
	Datastore string // default: the original datastore
	Overwrite bool
	Start     bool
	Progress  func(done, total uint64)
	Log       func(msg string, args ...any)
}

// RestoreResult describes the restored VM.
type RestoreResult struct {
	Name  string
	MoID  string
	VMX   string
	Disks []string
	Notes []string
}

var badNameChars = regexp.MustCompile(`[/\\:*?"<>|%\x00-\x1f]`)

// validName checks a VM name for ESXi.
func validName(n string) error {
	if n == "" || len(n) > 80 || badNameChars.MatchString(n) || strings.HasPrefix(n, ".") {
		return errors.New(`the VM name must have 1 – 80 characters and none of / \ : * ? " < > | %`)
	}
	return nil
}

// Restore recreates a VM of snapshot sn on the host.
func (c *Client) Restore(ctx context.Context, r *repo.Repository, sn *repo.Snapshot, opts RestoreOptions) (*RestoreResult, error) {
	if opts.Log == nil {
		opts.Log = func(string, ...any) {}
	}
	var g *repo.Guest
	for i := range sn.Guests {
		if sn.Guests[i].Platform == "vmware" && sn.Guests[i].VMID == opts.VMID {
			g = &sn.Guests[i]
		}
	}
	if g == nil {
		return nil, fmt.Errorf("VM %d is not in backup %s", opts.VMID, sn.ID.Short())
	}
	var cfg guestConfig
	if err := json.Unmarshal([]byte(g.Config), &cfg); err != nil || cfg.VMX == "" {
		return nil, errors.New("the backup has no VM configuration")
	}
	for _, d := range g.Disks {
		if d.Image < 0 || d.Image >= len(sn.Images) {
			return nil, fmt.Errorf("disk %s has no data in the backup", d.Key)
		}
	}
	if err := c.CheckSSH(ctx); err != nil {
		return nil, err
	}
	vms, err := c.VMs(ctx)
	if err != nil {
		return nil, err
	}
	var existing *VM
	for i := range vms {
		if strings.EqualFold(vms[i].UUID, g.ID) {
			existing = &vms[i]
		}
	}
	var total uint64
	for _, d := range g.Disks {
		total += d.Size
	}
	var done uint64
	progress := func(n uint64) {
		done += n
		if opts.Progress != nil {
			opts.Progress(done, max(total, done))
		}
	}
	if !opts.AsNew && existing != nil {
		if !opts.Overwrite {
			return nil, fmt.Errorf("VM %q still exists; restore it as a new VM, or confirm replacing its disks", existing.Name)
		}
		return c.restoreInPlace(ctx, r, sn, g, cfg, *existing, opts, progress)
	}
	name := opts.Name
	if name == "" {
		name = g.Name
		if opts.AsNew {
			name = g.Name + "-restored-" + time.Now().Format("20060102-1504")
		}
	}
	if err := validName(name); err != nil {
		return nil, err
	}
	for _, v := range vms {
		if strings.EqualFold(v.Name, name) {
			return nil, fmt.Errorf("a VM named %q already exists", name)
		}
	}
	return c.restoreNew(ctx, r, sn, g, cfg, name, opts, progress)
}

func adapterFor(key string) string {
	if strings.HasPrefix(key, "ide") {
		return "ide"
	}
	return "lsilogic"
}

// writeDisk creates a thin disk of the image's size at p and writes the
// image's data into it.
func (c *Client) writeDisk(ctx context.Context, r *repo.Repository, img *repo.DiskImage, p dsPath, key string, progress func(uint64)) error {
	if _, err := c.run(ctx, fmt.Sprintf("vmkfstools -c %d -d thin -a %s %s", img.Size, adapterFor(key), sq(p.local()))); err != nil {
		return fmt.Errorf("create disk: %w", err)
	}
	flat, err := c.flatFile(ctx, p)
	if err != nil {
		return err
	}
	w, err := c.openWriter(ctx, flat)
	if err != nil {
		return err
	}
	if err := diskimg.Write(ctx, r, img, w, false, progress); err != nil {
		w.Close()
		return err
	}
	return w.Close()
}

// exists reports whether a path exists on the host.
func (c *Client) exists(ctx context.Context, local string) bool {
	_, err := c.run(ctx, "test -e "+sq(local))
	return err == nil
}

func (c *Client) restoreNew(ctx context.Context, r *repo.Repository, sn *repo.Snapshot, g *repo.Guest, cfg guestConfig, name string,
	opts RestoreOptions, progress func(uint64)) (*RestoreResult, error) {
	orig, err := parseDSPath(cfg.VM.VMX)
	if err != nil {
		return nil, err
	}
	ds := opts.Datastore
	if ds == "" {
		ds = orig.DS
	}
	dir := badNameChars.ReplaceAllString(name, "_")
	for i := 2; c.exists(ctx, "/vmfs/volumes/"+ds+"/"+dir); i++ {
		dir = fmt.Sprintf("%s_%d", badNameChars.ReplaceAllString(name, "_"), i)
	}
	vmxPath := dsPath{DS: ds, Path: dir + "/" + dir + ".vmx"}
	res := &RestoreResult{Name: name, VMX: vmxPath.String()}
	if _, err := c.run(ctx, "mkdir "+sq("/vmfs/volumes/"+ds+"/"+dir)); err != nil {
		return nil, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			c.run(context.Background(), "rm -rf "+sq("/vmfs/volumes/"+ds+"/"+dir))
		}
	}()
	v := parseVMX([]byte(cfg.VMX))
	for i, d := range g.Disks {
		file := dir + ".vmdk"
		if i > 0 {
			file = fmt.Sprintf("%s_%d.vmdk", dir, i)
		}
		p := vmxPath.join(file)
		opts.Log("restoring disk", "key", d.Key, "file", p.String())
		img := sn.Images[d.Image]
		if err := c.writeDisk(ctx, r, &img, p, d.Key, progress); err != nil {
			return nil, fmt.Errorf("%s: %w", d.Key, err)
		}
		v.Set(d.Key+".fileName", file)
		res.Disks = append(res.Disks, p.String())
	}
	restored := map[string]bool{}
	for _, d := range g.Disks {
		restored[strings.ToLower(d.Key)] = true
	}
	for _, k := range v.Keys() {
		lk := strings.ToLower(k)
		if !strings.HasSuffix(lk, ".filename") || !strings.HasSuffix(strings.ToLower(v.Get(k)), ".vmdk") {
			continue
		}
		dev := k[:len(k)-len(".fileName")]
		if !restored[strings.ToLower(dev)] {
			v.Set(dev+".present", "FALSE")
			res.Notes = append(res.Notes, fmt.Sprintf("disk %s (%s) was not in the backup and was removed", dev, v.Get(k)))
		}
	}
	res.Notes = append(res.Notes, adaptVMX(v, name, dir, opts.AsNew)...)
	if len(cfg.NVRAM) > 0 {
		if err := c.put(ctx, vmxPath.join(dir+".nvram"), cfg.NVRAM); err != nil {
			return nil, fmt.Errorf("write NVRAM: %w", err)
		}
	}
	if nets, err := c.Networks(ctx); err == nil {
		res.Notes = append(res.Notes, checkNetworks(v, nets)...)
	}
	if err := c.put(ctx, vmxPath, v.Bytes()); err != nil {
		return nil, fmt.Errorf("write configuration: %w", err)
	}
	out, err := c.run(ctx, "vim-cmd solo/registervm "+sq(vmxPath.local())+" "+sq(name))
	if err != nil {
		return nil, fmt.Errorf("register VM: %w", err)
	}
	res.MoID = strings.TrimSpace(lastLines(out, 1))
	if _, err := strconv.Atoi(res.MoID); err != nil {
		return nil, fmt.Errorf("register VM: unexpected answer %q", out)
	}
	cleanup = false
	if opts.Start {
		if _, err := c.run(ctx, "vim-cmd vmsvc/power.on "+sq(res.MoID)); err != nil {
			res.Notes = append(res.Notes, "the VM could not be started: "+err.Error())
		}
	}
	return res, nil
}

var nicKeyRe = regexp.MustCompile(`^(ethernet\d+)\.present$`)

// adaptVMX prepares a backed-up configuration for a new VM: new name and
// files; for a copy also a new identity (UUIDs, MAC addresses).
func adaptVMX(v *vmx, name, base string, asNew bool) []string {
	var notes []string
	v.Set("displayName", name)
	v.Set("nvram", base+".nvram")
	for _, k := range []string{"sched.swap.derivedName", "migrate.hostLog", "checkpoint.vmState", "vmotion.checkpointFBSize",
		"vmotion.checkpointSVGAPrimarySize", "vmci0.id", "toolsInstallManager.lastInstallError"} {
		v.Delete(k)
	}
	v.DeletePrefix("migrate.")
	for _, k := range v.Keys() {
		lk := strings.ToLower(k)
		// ISO and floppy images lived next to the original VM and are not
		// part of the backup: leave the drives empty.
		if strings.HasSuffix(lk, ".devicetype") && strings.EqualFold(v.Get(k), "cdrom-image") {
			dev := k[:len(k)-len(".deviceType")]
			notes = append(notes, fmt.Sprintf("CD/DVD drive %s was connected to %q; it is left empty", dev, path.Base(v.Get(dev+".fileName"))))
			v.Set(k, "atapi-cdrom")
			v.Set(dev+".fileName", "emptyBackingString")
			v.Set(dev+".startConnected", "FALSE")
			v.Set(dev+".clientDevice", "TRUE")
		}
		if strings.HasPrefix(lk, "floppy") && strings.HasSuffix(lk, ".filetype") && strings.EqualFold(v.Get(k), "file") {
			v.Set(k[:len(k)-len(".fileType")]+".present", "FALSE")
		}
	}
	if asNew {
		v.Delete("uuid.bios")
		v.Delete("uuid.location")
		v.Delete("vc.uuid")
		for _, k := range v.Keys() {
			m := nicKeyRe.FindStringSubmatch(k)
			if m == nil {
				continue
			}
			nic := m[1]
			v.Delete(nic + ".generatedAddress")
			v.Delete(nic + ".generatedAddressOffset")
			v.Delete(nic + ".address")
			v.Set(nic+".addressType", "generated")
		}
		v.Set("answer.msg.uuid.altered", "I Copied It")
		notes = append(notes, "the copy has new MAC addresses; guest network settings bound to the old addresses do not apply")
	} else {
		v.Set("answer.msg.uuid.altered", "I Moved It")
	}
	return notes
}

func checkNetworks(v *vmx, nets []string) []string {
	have := map[string]bool{}
	for _, n := range nets {
		have[strings.ToLower(n)] = true
	}
	var notes []string
	for _, k := range v.Keys() {
		if m := nicKeyRe.FindStringSubmatch(k); m != nil {
			if n := v.Get(m[1] + ".networkName"); n != "" && !have[strings.ToLower(n)] {
				notes = append(notes, fmt.Sprintf("network %q of %s does not exist on this host; connect the adapter after the restore", n, m[1]))
			}
		}
	}
	return notes
}

// restoreInPlace replaces the disks of an existing VM with the backup. The
// VM keeps its identity, network adapters and MAC addresses; the old disks
// are kept renamed (.replaced-…).
func (c *Client) restoreInPlace(ctx context.Context, r *repo.Repository, sn *repo.Snapshot, g *repo.Guest, cfg guestConfig, vm VM,
	opts RestoreOptions, progress func(uint64)) (*RestoreResult, error) {
	res := &RestoreResult{Name: vm.Name, MoID: vm.MoID, VMX: vm.VMX}
	cur := map[string]Disk{}
	for _, d := range vm.Disks {
		cur[d.Key] = d
	}
	for _, d := range g.Disks {
		if _, ok := cur[d.Key]; !ok {
			return nil, fmt.Errorf("the VM has no disk %s any more; restore it as a new VM instead", d.Key)
		}
	}
	opts.Log("turning the VM off", "name", vm.Name)
	if vm.Power != "poweredOff" {
		if _, err := c.run(ctx, "vim-cmd vmsvc/power.off "+sq(vm.MoID)); err != nil {
			return nil, fmt.Errorf("turn off: %w", err)
		}
	}
	if vm.Snapshots {
		opts.Log("deleting the VM's snapshots")
		if _, err := c.run(ctx, "vim-cmd vmsvc/snapshot.removeall "+sq(vm.MoID)); err != nil {
			return nil, fmt.Errorf("delete snapshots: %w", err)
		}
		var err error
		if vm, err = c.vm(ctx, vm.Ref); err != nil {
			return nil, err
		}
		for _, d := range vm.Disks {
			cur[d.Key] = d
		}
	}
	stamp := time.Now().Format("20060102-150405")
	for _, d := range g.Disks {
		p, err := parseDSPath(cur[d.Key].File)
		if err != nil {
			return nil, err
		}
		old := p.join(strings.TrimSuffix(path.Base(p.Path), ".vmdk") + ".replaced-" + stamp + ".vmdk")
		opts.Log("restoring disk", "key", d.Key, "file", p.String())
		if _, err := c.run(ctx, "vmkfstools -E "+sq(p.local())+" "+sq(old.local())); err != nil {
			return nil, fmt.Errorf("%s: keep the old disk: %w", d.Key, err)
		}
		img := sn.Images[d.Image]
		if err := c.writeDisk(ctx, r, &img, p, d.Key, progress); err != nil {
			return nil, fmt.Errorf("%s: %w (the old disk is %s)", d.Key, err, old)
		}
		res.Disks = append(res.Disks, p.String())
		res.Notes = append(res.Notes, fmt.Sprintf("old disk kept as %s; delete it once the VM works", old))
	}
	// CPU and memory as in the backup.
	if vp, err := parseDSPath(vm.VMX); err == nil {
		if data, err := c.get(ctx, vp); err == nil {
			now, then := parseVMX(data), parseVMX([]byte(cfg.VMX))
			changed := false
			for _, k := range []string{"memSize", "numvcpus", "cpuid.coresPerSocket"} {
				if then.Has(k) && then.Get(k) != now.Get(k) {
					now.Set(k, then.Get(k))
					changed = true
				}
			}
			if changed {
				if err := c.put(ctx, vp, now.Bytes()); err == nil {
					c.run(ctx, "vim-cmd vmsvc/reload "+sq(vm.MoID))
				}
			}
		}
	}
	if opts.Start {
		if _, err := c.run(ctx, "vim-cmd vmsvc/power.on "+sq(vm.MoID)); err != nil {
			res.Notes = append(res.Notes, "the VM could not be started: "+err.Error())
		}
	}
	return res, nil
}
