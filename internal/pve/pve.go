// Package pve backs up and restores Proxmox VE virtual machines and
// containers without an agent inside the guests. It runs on a Proxmox node
// (as part of the BackupZit agent installed there) and uses the node's own
// tools: guest snapshots (qm/pct snapshot, which also freeze file systems
// through the QEMU guest agent), the storage layer (LVM-thin, ZFS, Ceph RBD,
// qcow2 files) to read the snapshot, and pvesm to allocate disks on restore.
//
// Disks are stored as fixed 1 MiB blocks, so unchanged blocks are not
// uploaded again and empty space costs nothing.
package pve

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// BlockSize is the unit of deduplication for guest disks.
const BlockSize = 1 << 20

// snapPrefix names the temporary guest snapshots taken for a backup.
const snapPrefix = "backupzit_"

// ConfigDir is where pmxcfs exposes the cluster configuration.
var ConfigDir = "/etc/pve"

// command runs a program and returns its standard output; tests replace it.
var command = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(string(out))
		}
		if msg != "" {
			return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, lastLines(msg, 3))
		}
		return out, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return out, nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " / ")
}

// Available reports whether this machine is a Proxmox VE node.
func Available() bool {
	_, err := os.Stat(filepath.Join(ConfigDir, "storage.cfg"))
	if err != nil {
		return false
	}
	_, err = exec.LookPath("qm")
	return err == nil
}

// Guest is a VM or container as listed in the inventory.
type Guest struct {
	VMID int `json:"vmid"`
	// ID is the hypervisor's own identifier when it is not a number
	// (Hyper-V VM GUID); VMID is then derived from it.
	ID       string `json:"id,omitempty"`
	Name     string `json:"name"`
	Type     string `json:"type"` // qemu | lxc
	Node     string `json:"node"`
	Status   string `json:"status"`
	MaxDisk  uint64 `json:"maxdisk"`
	MaxMem   uint64 `json:"maxmem"`
	Template bool   `json:"template,omitempty"`
}

// Inventory is what a node agent reports to the console.
type Inventory struct {
	// Platform is "proxmox" (also when empty) or "hyperv".
	Platform string   `json:"platform,omitempty"`
	Node     string   `json:"node"`
	Cluster  bool     `json:"cluster,omitempty"`
	Version  string   `json:"version,omitempty"`
	Guests   []Guest  `json:"guests"`
	Storage  []string `json:"storage"` // storages usable for guest disks on this node
}

// LocalNode returns the Proxmox node name of this machine.
func LocalNode() string {
	h, _ := os.Hostname()
	h, _, _ = strings.Cut(h, ".")
	return h
}

// GetInventory lists the guests of the cluster (or single node).
func GetInventory(ctx context.Context) (*Inventory, error) {
	out, err := command(ctx, "pvesh", "get", "/cluster/resources", "--type", "vm", "--output-format", "json")
	if err != nil {
		return nil, err
	}
	var raw []struct {
		VMID     int     `json:"vmid"`
		Name     string  `json:"name"`
		Type     string  `json:"type"`
		Node     string  `json:"node"`
		Status   string  `json:"status"`
		MaxDisk  float64 `json:"maxdisk"`
		MaxMem   float64 `json:"maxmem"`
		Template int     `json:"template"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse guest list: %w", err)
	}
	inv := &Inventory{Node: LocalNode(), Guests: []Guest{}}
	for _, g := range raw {
		inv.Guests = append(inv.Guests, Guest{VMID: g.VMID, Name: g.Name, Type: g.Type, Node: g.Node, Status: g.Status,
			MaxDisk: uint64(g.MaxDisk), MaxMem: uint64(g.MaxMem), Template: g.Template == 1})
	}
	sort.Slice(inv.Guests, func(i, j int) bool { return inv.Guests[i].VMID < inv.Guests[j].VMID })
	if v, err := command(ctx, "pveversion"); err == nil {
		inv.Version = strings.TrimSpace(string(v))
	}
	if _, err := os.Stat(filepath.Join(ConfigDir, "corosync.conf")); err == nil {
		inv.Cluster = true
	}
	if st, err := readStorage(); err == nil {
		for _, s := range st {
			if s.usable(inv.Node) && (s.has("images") || s.has("rootdir")) {
				inv.Storage = append(inv.Storage, s.ID)
			}
		}
	}
	return inv, nil
}

// ---- storage configuration

type storage struct {
	ID    string
	Type  string
	Props map[string]string
}

func (s storage) has(content string) bool {
	for _, c := range strings.Split(s.Props["content"], ",") {
		if strings.TrimSpace(c) == content {
			return true
		}
	}
	return false
}

func (s storage) usable(node string) bool {
	if s.Props["disable"] == "1" {
		return false
	}
	if n := s.Props["nodes"]; n != "" {
		for _, x := range strings.Split(n, ",") {
			if strings.TrimSpace(x) == node {
				return true
			}
		}
		return false
	}
	return true
}

// readStorage parses storage.cfg.
func readStorage() (map[string]storage, error) {
	b, err := os.ReadFile(filepath.Join(ConfigDir, "storage.cfg"))
	if err != nil {
		return nil, err
	}
	return parseStorage(string(b)), nil
}

func parseStorage(text string) map[string]storage {
	res := map[string]storage{}
	var cur *storage
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if line[0] != ' ' && line[0] != '\t' {
			typ, id, ok := strings.Cut(line, ":")
			if !ok {
				cur = nil
				continue
			}
			s := storage{ID: strings.TrimSpace(id), Type: strings.TrimSpace(typ), Props: map[string]string{}}
			res[s.ID] = s
			cur = &s
			continue
		}
		if cur == nil {
			continue
		}
		k, v, _ := strings.Cut(strings.TrimSpace(line), " ")
		res[cur.ID].Props[k] = strings.TrimSpace(v)
	}
	return res
}

// ---- guest configuration

// configPath returns the configuration file of a guest on any node.
func configPath(typ string, vmid int) (string, error) {
	dir := "qemu-server"
	if typ == "lxc" {
		dir = "lxc"
	}
	matches, _ := filepath.Glob(filepath.Join(ConfigDir, "nodes", "*", dir, strconv.Itoa(vmid)+".conf"))
	if len(matches) == 1 {
		return matches[0], nil
	}
	p := filepath.Join(ConfigDir, dir, strconv.Itoa(vmid)+".conf")
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("guest %d has no configuration", vmid)
}

// mainSection returns the configuration without snapshot and pending
// sections.
func mainSection(conf string) string {
	var b strings.Builder
	for _, line := range strings.Split(conf, "\n") {
		if strings.HasPrefix(line, "[") {
			break
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// snapshotNames lists the snapshot sections of a configuration.
func snapshotNames(conf string) []string {
	var names []string
	for _, line := range strings.Split(conf, "\n") {
		if strings.HasPrefix(line, "[") && strings.HasSuffix(strings.TrimSpace(line), "]") {
			n := strings.Trim(strings.TrimSpace(line), "[]")
			if !strings.Contains(n, ":") && n != "PENDING" {
				names = append(names, n)
			}
		}
	}
	return names
}

var (
	qemuDiskKey = regexp.MustCompile(`^(ide|sata|scsi|virtio)\d+$|^efidisk0$|^tpmstate0$`)
	lxcDiskKey  = regexp.MustCompile(`^rootfs$|^mp\d+$`)
)

type diskEntry struct {
	Key       string
	Volume    string // storage:volname
	Opts      string // everything after the volume
	CloudInit bool
}

// configDisks returns the disks of a guest configuration that are backed
// up, and the reasons for disks that are skipped.
func configDisks(typ, conf string) (disks []diskEntry, skipped []string) {
	for _, line := range strings.Split(mainSection(conf), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		re := qemuDiskKey
		if typ == "lxc" {
			re = lxcDiskKey
		}
		if !re.MatchString(k) {
			continue
		}
		vol, opts, _ := strings.Cut(v, ",")
		if typ == "lxc" && strings.HasPrefix(k, "mp") {
			// Container mount point values may be "volume=...".
			vol = strings.TrimPrefix(vol, "volume=")
		}
		optList := "," + opts + ","
		switch {
		case vol == "none":
			continue
		case strings.Contains(vol, "cloudinit"):
			disks = append(disks, diskEntry{Key: k, Volume: vol, Opts: opts, CloudInit: true})
			continue
		case strings.Contains(optList, ",media=cdrom,"):
			continue
		case strings.HasPrefix(vol, "/"):
			skipped = append(skipped, fmt.Sprintf("%s: %s is a device or bind mount, not a Proxmox volume", k, vol))
			continue
		case strings.Contains(optList, ",backup=0,"):
			skipped = append(skipped, fmt.Sprintf("%s: excluded from backups in the guest configuration (backup=0)", k))
			continue
		case !strings.Contains(vol, ":"):
			skipped = append(skipped, fmt.Sprintf("%s: %s is not a Proxmox volume", k, vol))
			continue
		}
		disks = append(disks, diskEntry{Key: k, Volume: vol, Opts: opts})
	}
	return disks, skipped
}

// parseSize parses Proxmox sizes such as 8G, 512M or 1048576.
func parseSize(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty size")
	}
	mult := uint64(1)
	switch s[len(s)-1] {
	case 'K', 'k':
		mult = 1 << 10
	case 'M', 'm':
		mult = 1 << 20
	case 'G', 'g':
		mult = 1 << 30
	case 'T', 't':
		mult = 1 << 40
	}
	if mult != 1 {
		s = s[:len(s)-1]
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	return uint64(f * float64(mult)), nil
}

// formatSize renders bytes the way Proxmox writes disk sizes.
func formatSize(b uint64) string {
	switch {
	case b%(1<<40) == 0:
		return fmt.Sprintf("%dT", b>>40)
	case b%(1<<30) == 0:
		return fmt.Sprintf("%dG", b>>30)
	case b%(1<<20) == 0:
		return fmt.Sprintf("%dM", b>>20)
	case b%(1<<10) == 0:
		return fmt.Sprintf("%dK", b>>10)
	}
	return strconv.FormatUint(b, 10)
}

func optValue(opts, key string) string {
	for _, o := range strings.Split(opts, ",") {
		if k, v, ok := strings.Cut(o, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// volumeFormat guesses the image format from a volume name.
func volumeFormat(volname string) string {
	for _, f := range []string{"qcow2", "vmdk", "raw"} {
		if strings.HasSuffix(volname, "."+f) {
			return f
		}
	}
	if strings.HasPrefix(volname, "subvol-") || strings.HasPrefix(filepath.Base(volname), "subvol-") {
		return "subvol"
	}
	return "raw"
}

// guestTool returns qm or pct.
func guestTool(typ string) string {
	if typ == "lxc" {
		return "pct"
	}
	return "qm"
}

// waitPath waits until a device node appears (udev).
func waitPath(ctx context.Context, p string) error {
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(p); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not appear", p)
		}
		command(ctx, "udevadm", "settle", "--timeout=5")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}
