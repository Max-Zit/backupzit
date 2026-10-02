// Package sysbackup backs up and restores whole Linux systems: all local
// file systems as files plus the disk layout (partitions, LVM, file system
// UUIDs, boot code), so the machine can be recreated on an empty disk or as
// a virtual machine. This is the approach of Relax-and-Recover: Linux has
// no built-in snapshot of a running disk, but a file backup of each file
// system is consistent per file and the layout makes it bootable again.
package sysbackup

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/backupzit/backupzit/internal/repo"
)

// Supported file systems; others (network, pseudo) are not part of a system
// backup.
var localFS = map[string]bool{"ext2": true, "ext3": true, "ext4": true, "xfs": true, "vfat": true, "btrfs": true}

// Bios boot partition (GRUB core image on GPT disks).
const biosBootType = "21686148-6449-6e6f-744e-656564454649"

type lsblkDev struct {
	Name        string     `json:"name"`
	Path        string     `json:"path"`
	Type        string     `json:"type"`
	Size        uint64     `json:"size"`
	FSType      string     `json:"fstype"`
	FSVer       string     `json:"fsver"`
	UUID        string     `json:"uuid"`
	PartUUID    string     `json:"partuuid"`
	Label       string     `json:"label"`
	PartLabel   string     `json:"partlabel"`
	PartType    string     `json:"parttype"`
	PartFlags   string     `json:"partflags"`
	PTType      string     `json:"pttype"`
	PTUUID      string     `json:"ptuuid"`
	MountPoints []*string  `json:"mountpoints"`
	LogSec      uint32     `json:"log-sec"`
	Model       string     `json:"model"`
	FSUsed      uint64     `json:"fsused"`
	Children    []lsblkDev `json:"children"`
}

func (d lsblkDev) mounts() []string {
	var out []string
	for _, m := range d.MountPoints {
		if m != nil && *m != "" {
			out = append(out, *m)
		}
	}
	return out
}

var command = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Capture records the layout of the disks that hold the mounted local file
// systems and returns the mount points to back up (in order).
func Capture(ctx context.Context, r *repo.Repository) (*repo.SystemLayout, []string, error) {
	out, err := command(ctx, "lsblk", "-J", "-b", "-o",
		"NAME,PATH,TYPE,SIZE,FSTYPE,FSVER,UUID,PARTUUID,LABEL,PARTLABEL,PARTTYPE,PARTFLAGS,PTTYPE,PTUUID,MOUNTPOINTS,LOG-SEC,MODEL,FSUSED")
	if err != nil {
		return nil, nil, err
	}
	var tree struct {
		BlockDevices []lsblkDev `json:"blockdevices"`
	}
	if err := json.Unmarshal(out, &tree); err != nil {
		return nil, nil, fmt.Errorf("parse lsblk: %w", err)
	}
	host, _ := os.Hostname()
	lay := &repo.SystemLayout{OS: osName(), Hostname: host, Arch: runtime.GOARCH}
	if _, err := os.Stat("/sys/firmware/efi"); err == nil {
		lay.UEFI = true
	}
	if k, err := command(ctx, "uname", "-r"); err == nil {
		lay.Kernel = strings.TrimSpace(string(k))
	}
	if b, err := os.ReadFile("/etc/fstab"); err == nil {
		lay.Fstab = string(b)
	}

	var paths []string
	vgs := map[string]bool{}
	for _, disk := range tree.BlockDevices {
		if disk.Type != "disk" || !holdsSystem(disk) {
			continue
		}
		sd := repo.SystemDisk{Name: disk.Name, Model: strings.TrimSpace(disk.Model), Size: disk.Size, SectorSize: max(disk.LogSec, 512),
			Table: disk.PTType, ID: disk.PTUUID}
		if sd.Table != "gpt" && sd.Table != "dos" {
			return nil, nil, fmt.Errorf("disk %s: partition table %q is not supported (GPT or MBR needed)", disk.Name, disk.PTType)
		}
		if head, err := readBlocks(disk.Path, 0, 1<<20); err == nil {
			if sd.Head, _, err = r.SaveBlob(ctx, repo.DataBlob, head); err != nil {
				return nil, nil, err
			}
		} else {
			return nil, nil, fmt.Errorf("read the start of %s: %w", disk.Path, err)
		}
		for _, part := range disk.Children {
			if part.Type != "part" {
				continue
			}
			sp, err := partitionInfo(part)
			if err != nil {
				return nil, nil, err
			}
			switch {
			case localFS[part.FSType] && len(part.mounts()) > 0:
				sp.Content = "fs"
				fs := fsInfo(ctx, part, part.Name)
				lay.FileSystems = append(lay.FileSystems, fs)
			case part.FSType == "swap":
				sp.Content = "swap"
				lay.Swap = append(lay.Swap, repo.SystemFS{Device: part.Name, Type: "swap", UUID: part.UUID, Label: part.Label, Size: part.Size})
			case part.FSType == "LVM2_member":
				sp.Content = "lvm"
				sp.PVUUID = part.UUID
				for _, lv := range part.Children {
					vg, lvName := lvNames(ctx, lv.Path)
					if vg == "" {
						continue
					}
					sp.VG = vg
					vgs[vg] = true
					dev := vg + "/" + lvName
					switch {
					case localFS[lv.FSType] && len(lv.mounts()) > 0:
						lay.FileSystems = append(lay.FileSystems, fsInfo(ctx, lv, dev))
					case lv.FSType == "swap":
						lay.Swap = append(lay.Swap, repo.SystemFS{Device: dev, Type: "swap", UUID: lv.UUID, Label: lv.Label, Size: lv.Size})
					}
				}
			case strings.EqualFold(part.PartType, biosBootType) || (part.FSType == "" && part.Size <= 16<<20):
				// BIOS boot partition (GRUB core) or other small raw area.
				sp.Content = "raw"
				if strings.EqualFold(part.PartType, biosBootType) {
					sp.Content = "bios_grub"
				}
				data, err := readBlocks(part.Path, 0, part.Size)
				if err != nil {
					return nil, nil, fmt.Errorf("read %s: %w", part.Path, err)
				}
				for off := 0; off < len(data); off += 1 << 20 {
					id, _, err := r.SaveBlob(ctx, repo.DataBlob, data[off:min(off+1<<20, len(data))])
					if err != nil {
						return nil, nil, err
					}
					sp.Raw = append(sp.Raw, id)
				}
			case part.FSType == "crypto_LUKS":
				return nil, nil, fmt.Errorf("%s is encrypted (LUKS); encrypted system disks are not supported yet", part.Path)
			case part.FSType == "linux_raid_member":
				return nil, nil, fmt.Errorf("%s is part of a software RAID; RAID systems are not supported yet", part.Path)
			}
			sd.Partitions = append(sd.Partitions, sp)
		}
		lay.Disks = append(lay.Disks, sd)
	}
	if len(lay.FileSystems) == 0 {
		return nil, nil, fmt.Errorf("no local file systems found")
	}
	for vg := range vgs {
		cfg, err := vgConfig(ctx, vg)
		if err != nil {
			return nil, nil, err
		}
		lay.VGs = append(lay.VGs, repo.SystemVG{Name: vg, Config: cfg})
	}
	sort.Slice(lay.VGs, func(i, j int) bool { return lay.VGs[i].Name < lay.VGs[j].Name })
	sort.SliceStable(lay.FileSystems, func(i, j int) bool { return len(lay.FileSystems[i].MountPoint) < len(lay.FileSystems[j].MountPoint) })
	for _, fs := range lay.FileSystems {
		if fs.Type == "btrfs" {
			return nil, nil, fmt.Errorf("%s is btrfs; btrfs systems are not supported yet", fs.MountPoint)
		}
		paths = append(paths, fs.MountPoint)
	}
	return lay, paths, nil
}

// holdsSystem reports whether a disk carries a mounted local file system
// (directly or through LVM).
func holdsSystem(d lsblkDev) bool {
	if localFS[d.FSType] && len(d.mounts()) > 0 {
		return true
	}
	for _, c := range d.Children {
		if holdsSystem(c) {
			return true
		}
	}
	return false
}

func partitionInfo(p lsblkDev) (repo.SystemPartition, error) {
	sys := filepath.Join("/sys/class/block", p.Name)
	num, err := readInt(filepath.Join(sys, "partition"))
	if err != nil {
		return repo.SystemPartition{}, fmt.Errorf("partition number of %s: %w", p.Name, err)
	}
	start, err := readInt(filepath.Join(sys, "start"))
	if err != nil {
		return repo.SystemPartition{}, fmt.Errorf("start of %s: %w", p.Name, err)
	}
	return repo.SystemPartition{Number: int(num), Start: uint64(start) * 512, Size: p.Size, Type: strings.ToLower(p.PartType),
		UUID: strings.ToLower(p.PartUUID), Name: p.PartLabel, Flags: p.PartFlags}, nil
}

func readInt(p string) (int64, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
}

func readBlocks(dev string, off int64, n uint64) ([]byte, error) {
	f, err := os.Open(dev)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, n)
	k, err := f.ReadAt(buf, off)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return buf[:k], nil
}

// fsInfo describes a mounted file system and how to recreate it.
func fsInfo(ctx context.Context, d lsblkDev, dev string) repo.SystemFS {
	m := d.mounts()
	sort.Slice(m, func(i, j int) bool { return len(m[i]) < len(m[j]) })
	fs := repo.SystemFS{Device: dev, Type: d.FSType, UUID: d.UUID, Label: d.Label, MountPoint: m[0], Size: d.Size, Used: d.FSUsed}
	switch d.FSType {
	case "ext2", "ext3", "ext4":
		if out, err := command(ctx, "tune2fs", "-l", d.Path); err == nil {
			var feats []string
			sc := bufio.NewScanner(bytes.NewReader(out))
			for sc.Scan() {
				k, v, ok := strings.Cut(sc.Text(), ":")
				if !ok {
					continue
				}
				v = strings.TrimSpace(v)
				switch strings.TrimSpace(k) {
				case "Filesystem features":
					for _, f := range strings.Fields(v) {
						switch f {
						case "needs_recovery", "orphan_present", "recover", "snapshot_bitmap", "mmp":
							continue
						}
						feats = append(feats, f)
					}
				case "Block size":
					fs.MkfsArgs = append(fs.MkfsArgs, "-b", v)
				case "Inode size":
					fs.MkfsArgs = append(fs.MkfsArgs, "-I", v)
				}
			}
			if len(feats) > 0 {
				fs.MkfsArgs = append(fs.MkfsArgs, "-O", "none,"+strings.Join(feats, ","))
			}
		}
	case "vfat":
		switch d.FSVer {
		case "FAT12":
			fs.MkfsArgs = []string{"-F", "12"}
		case "FAT16":
			fs.MkfsArgs = []string{"-F", "16"}
		default:
			fs.MkfsArgs = []string{"-F", "32"}
		}
	case "xfs":
		// Recreate with the on-disk features of the original so older
		// kernels and boot loaders can read it.
		if out, err := command(ctx, "xfs_info", m[0]); err == nil {
			s := string(out)
			var opts []string
			for _, f := range []string{"crc", "finobt", "rmapbt", "reflink", "bigtime", "inobtcount"} {
				if v := xfsFlag(s, f); v != "" {
					opts = append(opts, f+"="+v)
				}
			}
			if len(opts) > 0 {
				fs.MkfsArgs = []string{"-m", strings.Join(opts, ",")}
			}
		}
	}
	return fs
}

func xfsFlag(info, name string) string {
	i := strings.Index(info, name+"=")
	if i < 0 {
		return ""
	}
	v := info[i+len(name)+1:]
	if j := strings.IndexAny(v, " ,\n"); j >= 0 {
		v = v[:j]
	}
	return v
}

func lvNames(ctx context.Context, path string) (vg, lv string) {
	out, err := command(ctx, "lvs", "--noheadings", "-o", "vg_name,lv_name", path)
	if err != nil {
		return "", ""
	}
	f := strings.Fields(string(out))
	if len(f) != 2 {
		return "", ""
	}
	return f[0], f[1]
}

func vgConfig(ctx context.Context, vg string) (string, error) {
	tmp, err := os.CreateTemp("", "bz-vg-*")
	if err != nil {
		return "", err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	if _, err := command(ctx, "vgcfgbackup", "-f", tmp.Name(), vg); err != nil {
		return "", err
	}
	b, err := os.ReadFile(tmp.Name())
	return string(b), err
}

func osName() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "Linux"
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "PRETTY_NAME="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return "Linux"
}
