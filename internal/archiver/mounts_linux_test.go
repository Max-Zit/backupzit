//go:build linux

package archiver

import "testing"

// L-32 (0.55): kernel file systems are found by their type, also at mount
// points with escaped characters; real file systems and tmpfs are kept.
func TestParseMountinfo(t *testing.T) {
	info := `22 1 0:21 / /proc rw,nosuid,nodev,noexec,relatime shared:12 - proc proc rw
23 1 0:22 / /sys rw,nosuid,nodev,noexec,relatime shared:2 - sysfs sysfs rw
24 1 0:5 / /dev rw,nosuid shared:8 - devtmpfs udev rw,size=4000k
25 24 0:23 / /dev/pts rw,nosuid,noexec,relatime shared:9 - devpts devpts rw
26 1 253:0 / / rw,relatime shared:1 - ext4 /dev/mapper/root rw
27 1 0:25 / /run rw,nosuid,nodev,noexec,relatime shared:10 - tmpfs tmpfs rw
28 23 0:26 / /sys/fs/cgroup rw,nosuid shared:4 - cgroup2 cgroup2 rw
29 1 0:40 / /mnt/my\040proc rw shared:30 - proc proc rw
`
	m := parseMountinfo(info)
	for _, p := range []string{"/proc", "/sys", "/dev", "/dev/pts", "/sys/fs/cgroup", "/mnt/my proc"} {
		if !m[p] {
			t.Errorf("%s not recognized as a kernel file system", p)
		}
	}
	for _, p := range []string{"/", "/run"} {
		if m[p] {
			t.Errorf("%s is not a pseudo file system", p)
		}
	}
}

// P2 (0.55 review): with systemd automount a mount point has an autofs line
// and the real file system mounted over it; the one on top decides, so /boot
// and /home are read. An automount nobody triggered yet is read too (M-N12):
// the walk mounts it, as before 0.55.
func TestParseMountinfoAutomount(t *testing.T) {
	info := `26 1 253:0 / / rw,relatime shared:1 - ext4 /dev/mapper/root rw
40 26 0:41 / /boot rw,relatime shared:20 - autofs systemd-1 rw,fd=37,pgrp=1,timeout=120,minproto=5,maxproto=5,direct
41 40 8:1 / /boot rw,relatime shared:21 - vfat /dev/sda1 rw,fmask=0077,dmask=0077
42 26 0:42 / /home rw,relatime shared:22 - autofs systemd-1 rw,fd=38,direct
43 42 0:50 / /home rw,relatime shared:23 - nfs4 srv:/home rw,vers=4.2
44 26 0:43 / /efi rw,relatime shared:24 - autofs systemd-1 rw,fd=39,direct
45 23 0:44 / /proc/sys/fs/binfmt_misc rw,relatime shared:25 - autofs systemd-1 rw,fd=40,direct
46 45 0:45 / /proc/sys/fs/binfmt_misc rw,relatime shared:26 - binfmt_misc binfmt_misc rw
`
	m := parseMountinfo(info)
	for _, p := range []string{"/", "/boot", "/home", "/efi"} {
		if m[p] {
			t.Errorf("%s has a real file system on top but is treated as a kernel file system", p)
		}
	}
	for _, p := range []string{"/proc/sys/fs/binfmt_misc"} {
		if !m[p] {
			t.Errorf("%s not recognized as a kernel file system", p)
		}
	}
}
