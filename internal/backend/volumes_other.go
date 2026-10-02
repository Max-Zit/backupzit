//go:build !windows

package backend

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// listVolumes maps /dev/disk/by-label to mount points from /proc/mounts.
func listVolumes() ([]Volume, error) {
	mounts := map[string]string{} // device -> mount point
	if f, err := os.Open("/proc/mounts"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fs := strings.Fields(sc.Text())
			if len(fs) >= 2 {
				if _, ok := mounts[fs[0]]; !ok {
					mounts[fs[0]] = unescapeMount(fs[1])
				}
			}
		}
		f.Close()
	}
	entries, err := os.ReadDir("/dev/disk/by-label")
	if err != nil {
		return nil, nil
	}
	var out []Volume
	for _, e := range entries {
		dev, err := filepath.EvalSymlinks(filepath.Join("/dev/disk/by-label", e.Name()))
		if err != nil {
			continue
		}
		if mp, ok := mounts[dev]; ok {
			out = append(out, Volume{Label: unescapeMount(e.Name()), Path: mp})
		}
	}
	return out, nil
}

// unescapeMount decodes \040-style octal escapes (also \x20 in udev names).
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if s[i+1] == 'x' {
				if n, err := strconv.ParseUint(s[i+2:i+4], 16, 8); err == nil {
					b.WriteByte(byte(n))
					i += 3
					continue
				}
			} else if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
