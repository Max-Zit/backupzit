package sysbackup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/restorer"
)

// RestoreOptions control a system restore.
type RestoreOptions struct {
	// Target is the disk to overwrite: a block device (/dev/sdb), a logical
	// volume or a disk image file. Everything on it is lost.
	Target string
	// Disk selects the source disk by name (default: the disk with /).
	Disk string
	// NewHardware adapts the restored system to a different machine: network
	// settings bound to the old MAC addresses are changed to match any
	// wired interface, and the initramfs of dracut systems is rebuilt.
	NewHardware bool
	// DisableAgent keeps the BackupZit agent of the restored system from
	// starting with the identity of the original machine (for copies that
	// run next to it); it can be enrolled as a new machine.
	DisableAgent bool
	// RebuildInitramfs regenerates the initramfs with all drivers (dracut
	// --no-hostonly), needed when moving a RHEL-like system to different
	// hardware.
	RebuildInitramfs bool
	Log              func(string)
	Progress         func(path string, s *restorer.Stats)
}

// RestoreResult describes the restored system.
type RestoreResult struct {
	Target      string   `json:"target"`
	Partitions  int      `json:"partitions"`
	FileSystems []string `json:"file_systems"`
	Files       uint64   `json:"files"`
	Bytes       uint64   `json:"bytes"`
	Boot        string   `json:"boot"`
	Notes       []string `json:"notes,omitempty"`
	Errors      []string `json:"errors,omitempty"`
}

type restoreJob struct {
	ctx    context.Context
	r      *repo.Repository
	sn     *repo.Snapshot
	lay    *repo.SystemLayout
	disk   *repo.SystemDisk
	opts   RestoreOptions
	res    *RestoreResult
	dev    string // whole-disk device used for partitioning
	loop   string // loop device we attached ("" if none)
	mnt    string
	mounts []string // mounted paths, in mount order
	vgs    []string // activated volume groups
	parts  map[int]repo.SystemPartition
	sizes  map[int]uint64 // new partition sizes
}

func (j *restoreJob) log(format string, args ...any) {
	if j.opts.Log != nil {
		j.opts.Log(fmt.Sprintf(format, args...))
	}
}

// Restore recreates the system of sn on opts.Target.
func Restore(ctx context.Context, r *repo.Repository, sn *repo.Snapshot, opts RestoreOptions) (*RestoreResult, error) {
	if sn.System == nil {
		return nil, fmt.Errorf("snapshot %s is not a system backup", sn.ID.Short())
	}
	if os.Geteuid() != 0 {
		return nil, errors.New("a system restore must run as root")
	}
	j := &restoreJob{ctx: ctx, r: r, sn: sn, lay: sn.System, opts: opts, res: &RestoreResult{Target: opts.Target},
		parts: map[int]repo.SystemPartition{}, sizes: map[int]uint64{}}
	if err := j.pickDisk(); err != nil {
		return nil, err
	}
	defer j.cleanup()
	steps := []struct {
		name string
		fn   func() error
	}{
		{"check target", j.checkTarget},
		{"partition", j.partition},
		{"boot code", j.bootCode},
		{"LVM", j.lvm},
		{"file systems", j.mkfs},
		{"mount", j.mount},
		{"files", j.files},
		{"boot loader", j.bootloader},
	}
	for _, s := range steps {
		if err := ctx.Err(); err != nil {
			return j.res, err
		}
		j.log("%s", s.name)
		if err := s.fn(); err != nil {
			return j.res, fmt.Errorf("%s: %w", s.name, err)
		}
	}
	return j.res, nil
}

// pickDisk selects the source disk; systems spread over several disks are
// not supported yet.
func (j *restoreJob) pickDisk() error {
	if len(j.lay.Disks) == 0 {
		return errors.New("the backup has no disk layout")
	}
	if j.opts.Disk != "" {
		for i := range j.lay.Disks {
			if j.lay.Disks[i].Name == j.opts.Disk {
				j.disk = &j.lay.Disks[i]
			}
		}
		if j.disk == nil {
			return fmt.Errorf("the backup has no disk %s", j.opts.Disk)
		}
	} else {
		if len(j.lay.Disks) > 1 {
			return fmt.Errorf("the system used %d disks; restoring systems spread over several disks is not supported yet", len(j.lay.Disks))
		}
		j.disk = &j.lay.Disks[0]
	}
	for _, p := range j.disk.Partitions {
		j.parts[p.Number] = p
		j.sizes[p.Number] = p.Size
	}
	return nil
}

func (j *restoreJob) run(name string, args ...string) (string, error) {
	out, err := command(j.ctx, name, args...)
	return string(out), err
}

// checkTarget makes sure the target is not in use and large enough, and
// attaches files and logical volumes as loop devices so partitions appear.
func (j *restoreJob) checkTarget() error {
	if err := j.checkTools(); err != nil {
		return err
	}
	t := j.opts.Target
	fi, err := os.Stat(t)
	if err != nil {
		return err
	}
	if mounted, err := inUse(t); err != nil {
		return err
	} else if mounted != "" {
		return fmt.Errorf("%s is in use (%s); choose an empty disk", t, mounted)
	}
	isDisk := false
	if fi.Mode()&os.ModeDevice != 0 {
		out, err := j.run("lsblk", "-dno", "TYPE", t)
		isDisk = err == nil && strings.TrimSpace(out) == "disk"
	}
	if isDisk {
		j.dev = t
	} else {
		out, err := j.run("losetup", "--find", "--show", "--partscan", t)
		if err != nil {
			return err
		}
		j.loop = strings.TrimSpace(out)
		j.dev = j.loop
	}
	out, err := j.run("blockdev", "--getsize64", j.dev)
	if err != nil {
		return err
	}
	size, _ := strconv.ParseUint(strings.TrimSpace(out), 10, 64)
	return j.plan(size)
}

// inUse reports a mount or holder that uses the device or its partitions.
func inUse(dev string) (string, error) {
	real, err := filepath.EvalSymlinks(dev)
	if err != nil {
		return "", err
	}
	out, err := command(context.Background(), "lsblk", "-J", "-o", "NAME,MOUNTPOINTS,TYPE", real)
	if err != nil {
		return "", nil // not a block device (file)
	}
	var tree struct {
		BlockDevices []lsblkDev `json:"blockdevices"`
	}
	json.Unmarshal(out, &tree)
	var find func(d lsblkDev) string
	find = func(d lsblkDev) string {
		if m := d.mounts(); len(m) > 0 {
			return d.Name + " mounted at " + m[0]
		}
		for _, c := range d.Children {
			if s := find(c); s != "" {
				return s
			}
		}
		return ""
	}
	for _, d := range tree.BlockDevices {
		if s := find(d); s != "" {
			return s, nil
		}
	}
	return "", nil
}

// plan fits the partitions on a target of the given size: the last
// partition grows or shrinks with the disk (its file system is created new,
// so only the data has to fit).
func (j *restoreJob) plan(size uint64) error {
	nums := j.partNumbers()
	if len(nums) == 0 {
		return errors.New("the disk has no partitions")
	}
	last := j.parts[nums[len(nums)-1]]
	for _, n := range nums {
		if p := j.parts[n]; p.Start+p.Size > last.Start+last.Size {
			last = p
		}
	}
	avail := int64(size) - int64(last.Start) - 1<<20 // keep 1 MiB for the backup GPT
	need := last.Size
	if avail < int64(need)-64<<20 {
		minimum := j.minimumSize(last)
		if avail < int64(minimum) {
			return fmt.Errorf("target is too small: %s needed, %s available", human(last.Start+minimum+1<<20), human(size))
		}
		j.res.Notes = append(j.res.Notes, fmt.Sprintf("partition %d shrunk from %s to %s to fit the target", last.Number, human(last.Size), human(uint64(avail))))
	} else if avail > int64(need)+64<<20 {
		j.res.Notes = append(j.res.Notes, fmt.Sprintf("partition %d grown from %s to %s to use the target", last.Number, human(last.Size), human(uint64(avail))))
	}
	j.sizes[last.Number] = uint64(avail) &^ (1<<20 - 1)
	return nil
}

// minimumSize is what the last partition needs at least: the data of its
// file system with some room, or its full size when it cannot shrink.
func (j *restoreJob) minimumSize(p repo.SystemPartition) uint64 {
	if p.Content != "fs" {
		return p.Size
	}
	for _, f := range j.lay.FileSystems {
		if f.Device == j.partName(p) {
			return max(f.Used+f.Used/5+256<<20, 512<<20)
		}
	}
	return p.Size
}

func (j *restoreJob) partNumbers() []int {
	var nums []int
	for n := range j.parts {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	return nums
}

func (j *restoreJob) partName(p repo.SystemPartition) string {
	return partDevName(j.disk.Name, p.Number)
}

// partDevName returns the kernel name of partition n of disk.
func partDevName(disk string, n int) string {
	if disk != "" && disk[len(disk)-1] >= '0' && disk[len(disk)-1] <= '9' {
		return fmt.Sprintf("%sp%d", disk, n) // nvme0n1p1, loop0p1, mmcblk0p1
	}
	return fmt.Sprintf("%s%d", disk, n)
}

// partDev returns the target device of partition n.
func (j *restoreJob) partDev(n int) string {
	return "/dev/" + partDevName(filepath.Base(j.dev), n)
}

func (j *restoreJob) partition() error {
	nums := j.partNumbers()
	j.run("wipefs", "-a", j.dev)
	if j.disk.Table == "gpt" {
		if _, err := j.run("sgdisk", "--zap-all", j.dev); err != nil {
			return err
		}
		args := []string{"--clear"}
		if j.disk.ID != "" {
			args = append(args, "--disk-guid="+j.disk.ID)
		}
		for _, n := range nums {
			p := j.parts[n]
			first := p.Start / 512
			lastS := (p.Start+j.sizes[n])/512 - 1
			args = append(args, fmt.Sprintf("--new=%d:%d:%d", n, first, lastS), fmt.Sprintf("--typecode=%d:%s", n, p.Type))
			if p.UUID != "" {
				args = append(args, fmt.Sprintf("--partition-guid=%d:%s", n, p.UUID))
			}
			if p.Name != "" {
				args = append(args, fmt.Sprintf("--change-name=%d:%s", n, p.Name))
			}
			if bits, err := strconv.ParseUint(strings.TrimPrefix(p.Flags, "0x"), 16, 64); err == nil {
				for b := 0; b < 64; b++ {
					if bits&(1<<b) != 0 {
						args = append(args, fmt.Sprintf("--attributes=%d:set:%d", n, b))
					}
				}
			}
		}
		args = append(args, j.dev)
		if _, err := j.run("sgdisk", args...); err != nil {
			return err
		}
	} else {
		var b strings.Builder
		b.WriteString("label: dos\n")
		if j.disk.ID != "" {
			fmt.Fprintf(&b, "label-id: 0x%s\n", strings.TrimPrefix(j.disk.ID, "0x"))
		}
		b.WriteString("unit: sectors\n\n")
		for _, n := range nums {
			p := j.parts[n]
			if n > 4 {
				return errors.New("MBR disks with logical partitions are not supported yet")
			}
			fmt.Fprintf(&b, "%s : start=%d, size=%d, type=%s", j.partDev(n), p.Start/512, j.sizes[n]/512, strings.TrimPrefix(p.Type, "0x"))
			if p.Flags == "0x80" {
				b.WriteString(", bootable")
			}
			b.WriteString("\n")
		}
		f, err := os.CreateTemp("", "bz-sfdisk-*")
		if err != nil {
			return err
		}
		f.WriteString(b.String())
		f.Close()
		defer os.Remove(f.Name())
		in, err := os.Open(f.Name())
		if err != nil {
			return err
		}
		cmd := exec.CommandContext(j.ctx, "sfdisk", "--wipe", "always", j.dev)
		cmd.Stdin = in
		out, err := cmd.CombinedOutput()
		in.Close()
		if err != nil {
			return fmt.Errorf("sfdisk: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}
	j.run("partprobe", j.dev)
	j.run("udevadm", "settle")
	for _, n := range nums {
		if err := waitDev(j.ctx, j.partDev(n)); err != nil {
			return err
		}
	}
	j.res.Partitions = len(nums)
	return nil
}

func waitDev(ctx context.Context, p string) error {
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(p); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("%s did not appear", p)
}

// bootCode writes the BIOS boot code: the MBR code area and, on MBR disks,
// the gap before the first partition (GRUB core image); on GPT disks the
// BIOS boot partition is written in partition order below.
func (j *restoreJob) bootCode() error {
	head, err := j.r.LoadBlob(j.ctx, repo.DataBlob, j.disk.Head)
	if err != nil {
		return fmt.Errorf("load disk head: %w", err)
	}
	f, err := os.OpenFile(j.dev, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if len(head) >= 440 {
		if _, err := f.WriteAt(head[:440], 0); err != nil {
			return err
		}
	}
	if j.disk.Table == "dos" {
		first := uint64(len(head))
		for _, p := range j.parts {
			first = min(first, p.Start)
		}
		if first > 512 {
			if _, err := f.WriteAt(head[512:first], 512); err != nil {
				return err
			}
		}
	}
	if err := f.Sync(); err != nil {
		return err
	}
	for _, n := range j.partNumbers() {
		p := j.parts[n]
		if len(p.Raw) == 0 {
			continue
		}
		pf, err := os.OpenFile(j.partDev(n), os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		var off int64
		for _, id := range p.Raw {
			data, err := j.r.LoadBlob(j.ctx, repo.DataBlob, id)
			if err != nil {
				pf.Close()
				return err
			}
			if _, err := pf.WriteAt(data, off); err != nil {
				pf.Close()
				return err
			}
			off += int64(len(data))
		}
		pf.Sync()
		pf.Close()
	}
	return nil
}

func (j *restoreJob) lvm() error {
	used := map[string]bool{}
	for _, n := range j.partNumbers() {
		if p := j.parts[n]; p.Content == "lvm" && p.VG != "" {
			used[p.VG] = true
		}
	}
	for _, vg := range j.lay.VGs {
		if !used[vg.Name] {
			continue
		}
		if out, err := j.run("vgs", "--noheadings", "-o", "vg_name", vg.Name); err == nil && strings.TrimSpace(out) != "" {
			return fmt.Errorf("a volume group named %q already exists on this machine; restore on another machine or into a VM", vg.Name)
		}
		cfg, err := os.CreateTemp("", "bz-vgcfg-*")
		if err != nil {
			return err
		}
		cfg.WriteString(vg.Config)
		cfg.Close()
		defer os.Remove(cfg.Name())
		for _, n := range j.partNumbers() {
			p := j.parts[n]
			if p.Content != "lvm" || p.VG != vg.Name {
				continue
			}
			if _, err := j.run("pvcreate", "-ff", "-y", "--uuid", p.PVUUID, "--restorefile", cfg.Name(), j.partDev(n)); err != nil {
				return err
			}
		}
		if _, err := j.run("vgcfgrestore", "--force", "-f", cfg.Name(), vg.Name); err != nil {
			return err
		}
		if _, err := j.run("vgchange", "-ay", vg.Name); err != nil {
			return err
		}
		j.vgs = append(j.vgs, vg.Name)
	}
	j.run("udevadm", "settle")
	return nil
}

// fsDev maps a file system device of the layout to the target device.
func (j *restoreJob) fsDev(device string) (string, bool) {
	if strings.Contains(device, "/") { // vg/lv
		return "/dev/" + device, true
	}
	for _, n := range j.partNumbers() {
		if j.partName(j.parts[n]) == device {
			return j.partDev(n), true
		}
	}
	return "", false
}

func (j *restoreJob) mkfs() error {
	for _, fs := range append(append([]repo.SystemFS{}, j.lay.FileSystems...), j.lay.Swap...) {
		dev, ok := j.fsDev(fs.Device)
		if !ok {
			continue // on another disk
		}
		if err := waitDev(j.ctx, dev); err != nil {
			return err
		}
		var args []string
		var tool string
		switch fs.Type {
		case "ext2", "ext3", "ext4":
			tool = "mkfs." + fs.Type
			args = append([]string{"-F", "-q", "-U", fs.UUID}, fs.MkfsArgs...)
			if fs.Label != "" {
				args = append(args, "-L", fs.Label)
			}
		case "xfs":
			tool = "mkfs.xfs"
			m := "uuid=" + fs.UUID
			for i := 0; i+1 < len(fs.MkfsArgs); i += 2 {
				if fs.MkfsArgs[i] == "-m" {
					m += "," + fs.MkfsArgs[i+1]
				}
			}
			args = []string{"-f", "-q", "-m", m}
			if fs.Label != "" {
				args = append(args, "-L", fs.Label)
			}
		case "vfat":
			tool = "mkfs.vfat"
			args = append(append([]string{}, fs.MkfsArgs...), "-i", strings.ReplaceAll(fs.UUID, "-", ""))
			if fs.Label != "" {
				args = append(args, "-n", fs.Label)
			}
		case "swap":
			tool = "mkswap"
			args = []string{"-U", fs.UUID}
			if fs.Label != "" {
				args = append(args, "-L", fs.Label)
			}
		default:
			return fmt.Errorf("cannot create a %s file system", fs.Type)
		}
		args = append(args, dev)
		if out, err := j.run(tool, args...); err != nil {
			return fmt.Errorf("%s: %w %s", dev, err, out)
		}
		if fs.Type != "swap" {
			j.res.FileSystems = append(j.res.FileSystems, fmt.Sprintf("%s %s on %s", fs.MountPoint, fs.Type, dev))
		}
	}
	return nil
}

func (j *restoreJob) mount() error {
	dir, err := os.MkdirTemp("", "backupzit-restore-")
	if err != nil {
		return err
	}
	j.mnt = dir
	for _, fs := range j.lay.FileSystems { // sorted by mount point length
		dev, ok := j.fsDev(fs.Device)
		if !ok {
			continue
		}
		mp := filepath.Join(j.mnt, fs.MountPoint)
		if err := os.MkdirAll(mp, 0o755); err != nil {
			return err
		}
		if _, err := j.run("mount", "-t", fs.Type, dev, mp); err != nil {
			return err
		}
		j.mounts = append(j.mounts, mp)
	}
	return nil
}

func (j *restoreJob) files() error {
	st, err := restorer.Run(j.ctx, j.r, j.sn, restorer.Options{Target: j.mnt, Progress: j.opts.Progress})
	if st != nil {
		j.res.Files, j.res.Bytes = st.Files, st.Bytes
		if len(st.Errors) > 20 {
			j.res.Errors = append(st.Errors[:20], fmt.Sprintf("... and %d more", len(st.Errors)-20))
		} else {
			j.res.Errors = st.Errors
		}
	}
	return err
}

// bootloader makes the restored system bootable. BIOS: the boot code was
// written as blocks. UEFI: the firmware of a new machine has no boot entry,
// so the boot loader is also installed at the removable-media path
// \EFI\BOOT\BOOTX64.EFI, which every UEFI firmware starts.
func (j *restoreJob) bootloader() error {
	if j.opts.NewHardware {
		j.adaptNetwork()
	}
	if j.opts.DisableAgent {
		cfg := filepath.Join(j.mnt, "etc/backupzit/agent.json")
		if _, err := os.Stat(cfg); err == nil && os.Rename(cfg, cfg+".original") == nil {
			j.res.Notes = append(j.res.Notes, "the BackupZit agent in the restored system was unenrolled (it would report as the original machine); enroll it as a new machine if needed")
		}
	}
	if j.opts.RebuildInitramfs || j.opts.NewHardware {
		j.rebuildInitramfs()
	}
	if !j.lay.UEFI {
		j.res.Boot = "BIOS: boot code restored"
		return nil
	}
	var esp string
	for _, fs := range j.lay.FileSystems {
		if fs.Type == "vfat" && (fs.MountPoint == "/boot/efi" || fs.MountPoint == "/efi" || fs.MountPoint == "/boot") {
			esp = filepath.Join(j.mnt, fs.MountPoint)
		}
	}
	if esp == "" {
		j.res.Boot = "UEFI: no EFI system partition found"
		j.res.Notes = append(j.res.Notes, "no EFI system partition in the backup; the system may not boot")
		return nil
	}
	efi := findDirFold(esp, "EFI")
	if efi == "" {
		j.res.Boot = "UEFI: EFI directory missing"
		return nil
	}
	boot := findDirFold(efi, "BOOT")
	if boot != "" && findFileFold(boot, "BOOTX64.EFI") != "" {
		j.res.Boot = "UEFI: boot loader at the default path"
		return nil
	}
	entries, _ := os.ReadDir(efi)
	for _, e := range entries {
		if !e.IsDir() || strings.EqualFold(e.Name(), "BOOT") {
			continue
		}
		dir := filepath.Join(efi, e.Name())
		loader := findFileFold(dir, "shimx64.efi")
		if loader == "" {
			loader = findFileFold(dir, "grubx64.efi")
		}
		if loader == "" {
			continue
		}
		if boot == "" {
			boot = filepath.Join(efi, "BOOT")
			os.MkdirAll(boot, 0o755)
		}
		files, _ := os.ReadDir(dir)
		for _, f := range files {
			if f.Type().IsRegular() {
				copyFile(filepath.Join(dir, f.Name()), filepath.Join(boot, f.Name()))
			}
		}
		if err := copyFile(loader, filepath.Join(boot, "BOOTX64.EFI")); err != nil {
			return err
		}
		j.res.Boot = fmt.Sprintf("UEFI: %s installed as the default boot loader", filepath.Join("EFI", e.Name(), filepath.Base(loader)))
		return nil
	}
	j.res.Boot = "UEFI: no GRUB or shim found in the EFI partition"
	j.res.Notes = append(j.res.Notes, "the EFI partition has no known boot loader; add a boot entry in the firmware")
	return nil
}

// rebuildInitramfs runs dracut in the restored system so it contains all
// storage drivers (RHEL-like systems build host-only images).
func (j *restoreJob) rebuildInitramfs() {
	if _, err := os.Stat(filepath.Join(j.mnt, "usr/bin/dracut")); err != nil {
		return // Debian/Ubuntu include the common drivers by default
	}
	var binds []string
	for _, d := range []string{"dev", "proc", "sys", "run"} {
		p := filepath.Join(j.mnt, d)
		os.MkdirAll(p, 0o755)
		if _, err := j.run("mount", "--rbind", "/"+d, p); err == nil {
			binds = append(binds, p)
		}
	}
	if out, err := j.run("chroot", j.mnt, "dracut", "-f", "--regenerate-all", "--no-hostonly"); err != nil {
		j.res.Notes = append(j.res.Notes, "initramfs not rebuilt: "+strings.TrimSpace(out)+" "+err.Error())
	} else {
		j.res.Notes = append(j.res.Notes, "initramfs rebuilt with all drivers (dracut)")
	}
	for i := len(binds) - 1; i >= 0; i-- {
		j.run("umount", "-R", binds[i])
	}
}

func (j *restoreJob) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	j.ctx = ctx
	if len(j.mounts) > 0 {
		j.run("sync")
	}
	for i := len(j.mounts) - 1; i >= 0; i-- {
		if _, err := j.run("umount", j.mounts[i]); err != nil {
			j.run("umount", "-l", j.mounts[i])
		}
	}
	if j.mnt != "" {
		os.Remove(j.mnt) // empty once everything is unmounted
	}
	for _, vg := range j.vgs {
		j.run("vgchange", "-an", vg)
	}
	if j.loop != "" {
		j.run("losetup", "-d", j.loop)
	}
}

func findDirFold(dir, name string) string {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() && strings.EqualFold(e.Name(), name) {
			return filepath.Join(dir, e.Name())
		}
	}
	return ""
}

func findFileFold(dir, name string) string {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(e.Name(), name) {
			return filepath.Join(dir, e.Name())
		}
	}
	return ""
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o755)
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func human(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// adaptNetwork rewrites network settings that only apply to the old
// network cards: netplan "match: macaddress" (common in cloud images) and
// udev persistent-net rules.
func (j *restoreJob) adaptNetwork() {
	files, _ := filepath.Glob(filepath.Join(j.mnt, "etc/netplan/*.yaml"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var out []string
		changed := false
		for _, line := range strings.Split(string(b), "\n") {
			t := strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(t, "macaddress:"):
				indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
				out = append(out, indent+`name: "e*"`)
				changed = true
				continue
			case strings.HasPrefix(t, "set-name:"):
				changed = true
				continue
			}
			out = append(out, line)
		}
		if changed {
			fi, _ := os.Stat(f)
			os.WriteFile(f, []byte(strings.Join(out, "\n")), fi.Mode().Perm())
			j.res.Notes = append(j.res.Notes, fmt.Sprintf("%s: network settings now apply to any wired interface instead of the old MAC address", strings.TrimPrefix(f, j.mnt)))
		}
	}
	rules := filepath.Join(j.mnt, "etc/udev/rules.d/70-persistent-net.rules")
	if _, err := os.Stat(rules); err == nil {
		if os.Rename(rules, rules+".backupzit-old") == nil {
			j.res.Notes = append(j.res.Notes, "/etc/udev/rules.d/70-persistent-net.rules disabled (bound to the old network cards)")
		}
	}
	if b, err := os.ReadFile(filepath.Join(j.mnt, "etc/network/interfaces")); err == nil && strings.Contains(string(b), "hwaddress") {
		j.res.Notes = append(j.res.Notes, "/etc/network/interfaces refers to MAC addresses; check the network settings of the restored system")
	}
}

// checkTools makes sure the programs needed for this layout exist (a Linux
// live system may lack some) and names the packages to install.
func (j *restoreJob) checkTools() error {
	need := map[string]string{"wipefs": "util-linux", "blockdev": "util-linux", "losetup": "util-linux", "mount": "util-linux", "lsblk": "util-linux"}
	if j.disk.Table == "gpt" {
		need["sgdisk"] = "gdisk"
	} else {
		need["sfdisk"] = "fdisk"
	}
	for _, fs := range append(append([]repo.SystemFS{}, j.lay.FileSystems...), j.lay.Swap...) {
		switch fs.Type {
		case "ext2", "ext3", "ext4":
			need["mkfs."+fs.Type] = "e2fsprogs"
		case "xfs":
			need["mkfs.xfs"] = "xfsprogs"
		case "vfat":
			need["mkfs.vfat"] = "dosfstools"
		case "swap":
			need["mkswap"] = "util-linux"
		}
	}
	if len(j.lay.VGs) > 0 {
		need["pvcreate"], need["vgcfgrestore"], need["vgchange"] = "lvm2", "lvm2", "lvm2"
	}
	var missing []string
	pkgs := map[string]bool{}
	for tool, pkg := range need {
		if _, err := exec.LookPath(tool); err != nil {
			missing = append(missing, tool)
			pkgs[pkg] = true
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	var p []string
	for k := range pkgs {
		p = append(p, k)
	}
	sort.Strings(p)
	return fmt.Errorf("missing programs %s; install the packages %s (e.g. apt install %s)", strings.Join(missing, ", "), strings.Join(p, ", "), strings.Join(p, " "))
}
