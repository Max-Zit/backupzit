package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
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

var pkgVersionRe = regexp.MustCompile(`^backupzit-agent[-_](\d+)\.(\d+)\.(\d+)`)

// parseVersion reads "1.2.3" (with any suffix such as -legacy) into
// comparable numbers.
func parseVersion(v string) ([3]int, bool) {
	m := regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)`).FindStringSubmatch(v)
	if m == nil {
		return [3]int{}, false
	}
	var out [3]int
	for i := range out {
		out[i], _ = strconv.Atoi(m[i+1])
	}
	return out, true
}

func versionLess(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// agentUpdate is the newest installer that fits an agent and is newer than
// the version it runs.
type agentUpdate struct {
	File, Version string
}

// packageKind returns the installer kind an agent uses: "msi", "legacy.msi",
// "deb" or "rpm" ("" if it cannot be updated from the console).
func packageKind(a Agent) string {
	if a.Recovery || a.Arch != "amd64" {
		return ""
	}
	osName := strings.ToLower(a.OS)
	switch {
	case strings.HasPrefix(osName, "windows") || strings.Contains(osName, "windows"):
		if strings.HasSuffix(a.Version, "-legacy") {
			return "legacy.msi"
		}
		return "msi"
	case a.OS == "":
		return ""
	case strings.Contains(osName, "debian"), strings.Contains(osName, "ubuntu"), strings.Contains(osName, "proxmox"), strings.Contains(osName, "mint"):
		return "deb"
	case strings.Contains(osName, "alma"), strings.Contains(osName, "rocky"), strings.Contains(osName, "red hat"), strings.Contains(osName, "centos"),
		strings.Contains(osName, "fedora"), strings.Contains(osName, "oracle"), strings.Contains(osName, "suse"):
		return "rpm"
	}
	return ""
}

func fileKind(name string) string {
	n := strings.ToLower(name)
	switch {
	case !strings.HasPrefix(n, "backupzit-agent"):
		return ""
	case strings.HasSuffix(n, "-legacy.msi"):
		return "legacy.msi"
	case strings.HasSuffix(n, ".msi"):
		return "msi"
	case strings.HasSuffix(n, ".deb"):
		return "deb"
	case strings.HasSuffix(n, ".rpm"):
		return "rpm"
	}
	return ""
}

// availableUpdates maps agent IDs to the update offered for them.
func (s *Server) availableUpdates(agents []Agent) map[int64]agentUpdate {
	out := map[int64]agentUpdate{}
	if s.DistDir == "" {
		return out
	}
	entries, err := os.ReadDir(s.DistDir)
	if err != nil {
		return out
	}
	type pkg struct {
		name string
		v    [3]int
	}
	best := map[string]pkg{}
	for _, e := range entries {
		k := fileKind(e.Name())
		m := pkgVersionRe.FindStringSubmatch(e.Name())
		if k == "" || m == nil || !e.Type().IsRegular() {
			continue
		}
		v, _ := parseVersion(m[1] + "." + m[2] + "." + m[3])
		if old, ok := best[k]; !ok || versionLess(old.v, v) {
			best[k] = pkg{e.Name(), v}
		}
	}
	for _, a := range agents {
		p, ok := best[packageKind(a)]
		if !ok {
			continue
		}
		cur, ok := parseVersion(a.Version)
		if !ok || versionLess(cur, p.v) {
			out[a.ID] = agentUpdate{File: p.name, Version: fmt.Sprintf("%d.%d.%d", p.v[0], p.v[1], p.v[2])}
		}
	}
	return out
}

// fileSHA256 hashes an installer, caching by name, size and time.
func (s *Server) fileSHA256(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	key := fmt.Sprintf("%s|%d|%d", path, fi.Size(), fi.ModTime().UnixNano())
	if v, ok := s.shaCache.Load(key); ok {
		return v.(string), nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	s.shaCache.Store(key, sum)
	return sum, nil
}
