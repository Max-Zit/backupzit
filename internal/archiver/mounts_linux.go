//go:build linux

package archiver

import (
	"os"
	"strconv"
	"strings"
)

// pseudoFS are kernel file systems without data to back up: reading them
// can block (/proc/kmsg) or produce endless or huge files (/proc/kcore).
// Not autofs: an automount point nobody used yet holds no data until it is
// read, and reading it mounts the real file system (NAS, /boot/efi; M-N12).
var pseudoFS = map[string]bool{
	"proc": true, "sysfs": true, "devtmpfs": true, "devpts": true, "cgroup": true, "cgroup2": true,
	"securityfs": true, "debugfs": true, "tracefs": true, "pstore": true, "bpf": true, "configfs": true,
	"fusectl": true, "mqueue": true, "hugetlbfs": true, "binfmt_misc": true,
	"efivarfs": true, "rpc_pipefs": true, "nsfs": true, "selinuxfs": true,
}

// pseudoMounts lists the mount points of pseudo file systems, from
// /proc/self/mountinfo (L-32: a file job with source / read /proc).
func pseudoMounts() map[string]bool {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	return parseMountinfo(string(b))
}

// parseMountinfo judges each mount point by its last listed entry, the
// mount on top that a reader sees: with systemd automount /boot or /home
// has an autofs line and, once used, the real file system mounted over it,
// and that one must be backed up.
func parseMountinfo(s string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(s, "\n") {
		pre, post, ok := strings.Cut(line, " - ")
		f, g := strings.Fields(pre), strings.Fields(post)
		if !ok || len(f) < 5 || len(g) < 1 {
			continue
		}
		if p := unescapeMount(f[4]); pseudoFS[g[0]] {
			out[p] = true
		} else {
			delete(out, p)
		}
	}
	return out
}

// unescapeMount decodes the octal escapes of mountinfo (\040 for a space).
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
