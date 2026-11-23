package pve

import (
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/repo"
)

// Settings that act on the host are kept, as Proxmox VE's own restore
// does, and named in the notes so that a backup from another (or a
// compromised) host cannot add a hook script or host mount unnoticed.
func TestRestoreConfigNamesHostSettings(t *testing.T) {
	ct := &repo.Guest{Type: "lxc", VMID: 200, Config: "arch: amd64\nhostname: ct\nhookscript: local:snippets/hook.pl\nlxc.mount.entry: /srv srv none bind 0 0\nlxc.cgroup2.devices.allow: c 226:* rwm\ndev0: /dev/dri/card0\nrootfs: local-lvm:vm-200-disk-0,size=8G\n",
		Disks: []repo.GuestDisk{{Key: "rootfs", Volume: "local-lvm:vm-200-disk-0", Size: 8 << 30}}}
	conf, _, notes := restoreConfig(ct, map[string]string{"rootfs": "local-lvm:vm-201-disk-0"}, "", true)
	if !strings.Contains(conf, "lxc.mount.entry: /srv srv none bind 0 0\n") || !strings.Contains(conf, "hookscript: local:snippets/hook.pl\n") {
		t.Errorf("host settings not kept:\n%s", conf)
	}
	if n := strings.Join(notes, "\n"); !strings.Contains(n, "act on this host: hookscript, lxc.mount.entry, lxc.cgroup2.devices.allow, dev0;") {
		t.Errorf("notes = %q", notes)
	}
	vm := &repo.Guest{Type: "qemu", VMID: 100, Config: "name: vm\nargs: -fw_cfg name=opt/x,file=/etc/shadow\nhostpci0: 0000:01:00.0\nusb0: host=1234:5678\nscsi0: local-lvm:vm-100-disk-0,size=8G\n",
		Disks: []repo.GuestDisk{{Key: "scsi0", Volume: "local-lvm:vm-100-disk-0", Size: 8 << 30}}}
	if _, _, notes := restoreConfig(vm, map[string]string{"scsi0": "local-lvm:vm-101-disk-0"}, "", true); !strings.Contains(strings.Join(notes, "\n"), "act on this host: args, hostpci0, usb0;") {
		t.Errorf("VM notes = %q", notes)
	}
	plain := &repo.Guest{Type: "qemu", VMID: 100, Config: "name: vm\nscsi0: local-lvm:vm-100-disk-0,size=8G\n",
		Disks: []repo.GuestDisk{{Key: "scsi0", Volume: "local-lvm:vm-100-disk-0", Size: 8 << 30}}}
	if _, _, notes := restoreConfig(plain, map[string]string{"scsi0": "local-lvm:vm-101-disk-0"}, "", true); len(notes) != 0 {
		t.Errorf("plain VM notes = %q", notes)
	}
}
