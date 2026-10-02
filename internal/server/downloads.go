package server

import (
	"os"
	"sort"
	"strings"
	"time"
)

type download struct {
	Name, Size  string
	Title, Desc string
	Primary     bool
	order       int
	mod         time.Time
}

// classify describes a file in the dist directory for the Agents page.
// Unknown files and server packages are not offered.
func classify(name string) (title, desc string, order int, primary, ok bool) {
	n := strings.ToLower(name)
	switch {
	case strings.HasSuffix(n, "-legacy.msi"):
		return "Windows agent — older systems", "MSI installer for Windows 7, Server 2008 R2 and 2012 R2", 2, false, true
	case strings.HasPrefix(n, "backupzit-agent") && strings.HasSuffix(n, ".msi"):
		return "Windows agent", "MSI installer for Windows 10, 11 and Server 2016 or newer", 1, true, true
	case strings.HasPrefix(n, "backupzit-agent") && strings.HasSuffix(n, ".deb"):
		return "Linux agent — Debian / Ubuntu", "deb package", 3, false, true
	case strings.HasPrefix(n, "backupzit-agent") && strings.HasSuffix(n, ".rpm"):
		return "Linux agent — AlmaLinux / Rocky Linux", "rpm package", 4, false, true
	case strings.HasPrefix(n, "backupzit-repo") && (strings.HasSuffix(n, ".deb") || strings.HasSuffix(n, ".rpm")):
		return "Hardened repository", "for a dedicated Linux backup server (" + n[strings.LastIndex(n, ".")+1:] + ")", 6, false, true
	case n == "backupzit-agent.exe":
		return "Command line agent", "advanced: standalone program for scripts and restores without the console — not an installer", 7, false, true
	case strings.HasSuffix(n, ".iso"):
		return "Recovery ISO", "bootable media for bare-metal restore", 5, false, true
	}
	return "", "", 0, false, false
}

// downloads lists the newest file of each kind.
func (s *Server) downloads() []download {
	if s.DistDir == "" {
		return nil
	}
	entries, err := os.ReadDir(s.DistDir)
	if err != nil {
		return nil
	}
	newest := map[string]download{}
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		title, desc, order, primary, ok := classify(e.Name())
		if !ok {
			continue
		}
		d := download{Name: e.Name(), Size: humanBytes(uint64(fi.Size())), Title: title, Desc: desc, Primary: primary, order: order, mod: fi.ModTime()}
		key := title + desc
		if old, ok := newest[key]; !ok || d.mod.After(old.mod) {
			newest[key] = d
		}
	}
	var out []download
	for _, d := range newest {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].order < out[j].order })
	return out
}
