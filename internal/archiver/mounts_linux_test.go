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
