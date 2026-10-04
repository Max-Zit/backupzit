package pve

import (
	"strings"
	"testing"

	"github.com/backupzit/backupzit/internal/repo"
)

const qemuConf = `agent: enabled=1
boot: order=scsi0
ide2: local-lvm:vm-100-cloudinit,media=cdrom
ide0: local:iso/debian.iso,media=cdrom
memory: 1024
name: deb-vm
net0: virtio=BC:24:11:DB:BB:E9,bridge=vmbr0
parent: before-upgrade
scsi0: local-lvm:vm-100-disk-0,size=8G
scsi1: local-lvm:vm-100-disk-1,backup=0,size=100G
scsi2: /dev/disk/by-id/ata-XYZ,size=500G
efidisk0: local-lvm:vm-100-disk-2,efitype=4m,size=4M
vmgenid: ec511402-8209-41e2-a98a-f6812a0942a4

[before-upgrade]
scsi0: local-lvm:vm-100-disk-0,size=4G
snaptime: 1790000000

[backupzit_1790000001]
snaptime: 1790000001
`

func TestConfigDisks(t *testing.T) {
	disks, skipped := configDisks("qemu", qemuConf)
	var keys []string
	for _, d := range disks {
		keys = append(keys, d.Key)
	}
	if got := strings.Join(keys, ","); got != "ide2,scsi0,efidisk0" {
		t.Fatalf("disks = %s", got)
	}
	if !disks[0].CloudInit || disks[1].CloudInit {
		t.Fatalf("cloud-init detection wrong: %+v", disks)
	}
	if len(skipped) != 2 || !strings.Contains(skipped[0], "backup=0") || !strings.Contains(skipped[1], "device") {
		t.Fatalf("skipped = %q", skipped)
	}
	if n := snapshotNames(qemuConf); len(n) != 2 || n[1] != "backupzit_1790000001" {
		t.Fatalf("snapshots = %v", n)
	}
	clean := cleanConfig(qemuConf)
	if strings.Contains(clean, "parent:") || strings.Contains(clean, "snaptime") || !strings.Contains(clean, "scsi0: local-lvm:vm-100-disk-0,size=8G") {
		t.Fatalf("cleanConfig:\n%s", clean)
	}

	lxc := "arch: amd64\nhostname: ct\nrootfs: local-lvm:vm-200-disk-0,size=4G\nmp0: local-lvm:vm-200-disk-1,mp=/data,size=10G\nmp1: /mnt/host,mp=/host\n"
	disks, skipped = configDisks("lxc", lxc)
	if len(disks) != 2 || disks[1].Key != "mp0" || len(skipped) != 1 {
		t.Fatalf("lxc disks %+v skipped %v", disks, skipped)
	}
}

func TestParseStorage(t *testing.T) {
	st := parseStorage("dir: local\n\tpath /var/lib/vz\n\tcontent iso,vztmpl,backup\n\nlvmthin: local-lvm\n\tthinpool data\n\tvgname pve\n\tcontent rootdir,images\n\nzfspool: tank\n\tpool tank/data\n\tcontent images\n\tnodes pve2\n")
	if st["local-lvm"].Type != "lvmthin" || st["local-lvm"].Props["vgname"] != "pve" || !st["local-lvm"].has("images") {
		t.Fatalf("local-lvm: %+v", st["local-lvm"])
	}
	if st["local"].has("images") {
		t.Fatal("local has no images")
	}
	if st["tank"].usable("pve") || !st["tank"].usable("pve2") {
		t.Fatal("nodes restriction ignored")
	}
	if !snapshotCapable(st["local-lvm"], "vm-100-disk-0") || snapshotCapable(st["local"], "100/vm-100-disk-0.raw") ||
		!snapshotCapable(st["local"], "100/vm-100-disk-0.qcow2") || snapshotCapable(st["tank"], "subvol-200-disk-0") {
		t.Fatal("snapshotCapable wrong")
	}
}

func TestSizes(t *testing.T) {
	for in, want := range map[string]uint64{"8G": 8 << 30, "4M": 4 << 20, "512": 512, "1.5G": 3 << 29, "2T": 2 << 40} {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v", in, got, err)
		}
	}
	for in, want := range map[uint64]string{8 << 30: "8G", 4 << 20: "4M", 1536 << 20: "1536M", 1000: "1000"} {
		if got := formatSize(in); got != want {
			t.Errorf("formatSize(%d) = %s", in, got)
		}
	}
}

func TestRestoreConfig(t *testing.T) {
	g := &repo.Guest{Type: "qemu", VMID: 100, Config: cleanConfig(qemuConf), Disks: []repo.GuestDisk{
		{Key: "scsi0", Volume: "local-lvm:vm-100-disk-0", Storage: "local-lvm", Size: 8 << 30},
		{Key: "efidisk0", Volume: "local-lvm:vm-100-disk-2", Storage: "local-lvm", Size: 4 << 20},
	}}
	conf, ci, notes := restoreConfig(g, map[string]string{"scsi0": "ceph:vm-150-disk-0", "efidisk0": "ceph:vm-150-disk-1"}, "copy", true)
	for _, want := range []string{"scsi0: ceph:vm-150-disk-0,size=8G\n", "efidisk0: ceph:vm-150-disk-1,efitype=4m,size=4M\n", "name: copy\n", "ide0: local:iso/debian.iso,media=cdrom\n"} {
		if !strings.Contains(conf, want) {
			t.Errorf("config lacks %q:\n%s", want, conf)
		}
	}
	for _, bad := range []string{"BC:24:11:DB:BB:E9", "ec511402", "vm-100-disk-1", "ata-XYZ", "cloudinit"} {
		if strings.Contains(conf, bad) {
			t.Errorf("config still contains %q:\n%s", bad, conf)
		}
	}
	if ci["ide2"] != "local-lvm" {
		t.Errorf("cloud-init drives = %v", ci)
	}
	if len(notes) != 2 {
		t.Errorf("notes = %v", notes)
	}
	// Unused disks, the snapshot parent and snapshot sections belong to the
	// original: deleting the copy must never delete the original's volumes.
	g2 := *g
	g2.Config = "name: deb-vm\nscsi0: local-lvm:vm-100-disk-0,size=8G\nunused0: local-lvm:vm-100-disk-9\nparent: before-update\n\n[before-update]\nscsi0: local-lvm:vm-100-disk-0,size=8G\nunused1: local-lvm:vm-100-disk-8\n"
	conf, _, _ = restoreConfig(&g2, map[string]string{"scsi0": "local-lvm:vm-150-disk-0"}, "", true)
	for _, bad := range []string{"unused", "parent", "[before-update]", "vm-100-disk"} {
		if strings.Contains(conf, bad) {
			t.Errorf("config of the copy contains %q:\n%s", bad, conf)
		}
	}
	// Restoring in place keeps MAC addresses.
	conf, _, _ = restoreConfig(g, map[string]string{"scsi0": "local-lvm:vm-100-disk-0", "efidisk0": "local-lvm:vm-100-disk-1"}, "", false)
	if !strings.Contains(conf, "virtio=BC:24:11:DB:BB:E9,bridge=vmbr0") || !strings.Contains(conf, "name: deb-vm") {
		t.Errorf("in-place restore changed identity:\n%s", conf)
	}
}
