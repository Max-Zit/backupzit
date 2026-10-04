package pve

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/backupzit/backupzit/internal/repo"
)

// Instant recovery: a VM is started directly from a backup. An NBD server
// (backupzit-agent nbd-serve, run as its own systemd unit) serves the
// backed-up disks read-only on a unix socket; each VM disk is a qcow2
// overlay on the "backupzit-instant" directory storage whose backing file
// is that NBD disk, so the VM boots in seconds and its writes stay local.
// Finish moves the disks to their final storage while the VM runs
// (qm disk move); Discard deletes the VM.

const (
	InstantStorage = "backupzit-instant"
	instantDir     = "/var/lib/backupzit-instant"
	// InstantRunDir holds the NBD sockets and the credentials of the units.
	InstantRunDir = "/run/backupzit-instant"
)

// InstantUnit is the systemd unit serving the disks of instant VM vmid.
func InstantUnit(vmid int) string { return fmt.Sprintf("backupzit-instant-%d", vmid) }

// InstantSocket is the NBD socket of instant VM vmid.
func InstantSocket(vmid int) string { return filepath.Join(InstantRunDir, fmt.Sprintf("%d.sock", vmid)) }

// InstantOptions start an instant recovery.
type InstantOptions struct {
	VMID    int    // guest in the backup
	NewVMID int    // -1: next free ID; 0: the original ID (only if it is free)
	Name    string // optional
	Start   bool
	// ServeCommand starts the NBD server unit for the new VM ID and the
	// socket path; it must return once the unit was started.
	ServeCommand func(ctx context.Context, vmid int, socket string) error
	Log          func(msg string, args ...any)
}

// ensureInstantStorage adds the directory storage for overlays on this node.
func ensureInstantStorage(ctx context.Context, node string) error {
	stores, err := readStorage()
	if err != nil {
		return err
	}
	if st, ok := stores[InstantStorage]; ok {
		if !st.has("images") {
			return fmt.Errorf("storage %s exists but does not hold VM disks", InstantStorage)
		}
		return nil
	}
	if err := os.MkdirAll(instantDir, 0o700); err != nil {
		return err
	}
	_, err = command(ctx, "pvesm", "add", "dir", InstantStorage, "--path", instantDir, "--content", "images", "--nodes", node)
	return err
}

// InstantStart creates and (with Start) starts a VM that runs from the backup.
func InstantStart(ctx context.Context, sn *repo.Snapshot, opts InstantOptions) (*RestoreResult, error) {
	if opts.Log == nil {
		opts.Log = func(string, ...any) {}
	}
	g, err := FindGuest(sn, opts.VMID)
	if err != nil {
		return nil, err
	}
	if g.Type != "qemu" {
		return nil, errors.New("instant recovery is available for VMs, not for containers")
	}
	inv, err := GetInventory(ctx)
	if err != nil {
		return nil, err
	}
	target := opts.NewVMID
	if target < 0 {
		if target, err = NextID(ctx); err != nil {
			return nil, err
		}
	} else if target == 0 {
		target = g.VMID
	}
	clone := false
	for _, x := range inv.Guests {
		if x.VMID == target {
			return nil, fmt.Errorf("guest %d exists; instant recovery creates a new guest", target)
		}
		clone = clone || x.VMID == g.VMID
	}
	if err := ensureInstantStorage(ctx, inv.Node); err != nil {
		return nil, fmt.Errorf("storage for instant recovery: %w", err)
	}
	if err := os.MkdirAll(InstantRunDir, 0o700); err != nil {
		return nil, err
	}
	sock := InstantSocket(target)
	os.Remove(sock)
	opts.Log("starting the disk server", "vmid", target, "socket", sock)
	if err := opts.ServeCommand(ctx, target, sock); err != nil {
		return nil, fmt.Errorf("start the disk server: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			command(context.Background(), "systemctl", "stop", InstantUnit(target))
		}
	}()
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if _, err := os.Stat(sock); err != nil {
		return nil, errors.New("the disk server did not start (journalctl -u " + InstantUnit(target) + ")")
	}
	res := &RestoreResult{VMID: target, Type: g.Type, Name: g.Name, Node: inv.Node}
	dir := filepath.Join(instantDir, "images", strconv.Itoa(target))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	defer func() {
		if !ok {
			os.RemoveAll(dir)
		}
	}()
	newVol := map[string]string{}
	for i, d := range g.Disks {
		backing := "nbd+unix:///" + d.Key + "?socket=" + sock
		if d.Key == "efidisk0" || d.Key == "tpmstate0" {
			// Firmware variables and TPM state are tiny and used as flash or
			// by swtpm, which need plain files: copy them.
			file := fmt.Sprintf("vm-%d-disk-%d.raw", target, i)
			p := filepath.Join(dir, file)
			if _, err := command(ctx, "qemu-img", "convert", "-f", "raw", "-O", "raw", backing, p); err != nil {
				return nil, fmt.Errorf("%s: copy: %w", d.Key, err)
			}
			if d.Key == "efidisk0" {
				// OVMF needs the variable store at its exact size.
				if err := os.Truncate(p, efiVarsSize(g.Config)); err != nil {
					return nil, err
				}
			}
			newVol[d.Key] = InstantStorage + ":" + strconv.Itoa(target) + "/" + file
			res.Disks = append(res.Disks, d.Key+" → "+newVol[d.Key]+" (copied)")
			continue
		}
		file := fmt.Sprintf("vm-%d-disk-%d.qcow2", target, i)
		if _, err := command(ctx, "qemu-img", "create", "-q", "-f", "qcow2", "-F", "raw", "-b", backing,
			filepath.Join(dir, file), strconv.FormatUint(d.Size, 10)); err != nil {
			return nil, fmt.Errorf("%s: overlay: %w", d.Key, err)
		}
		newVol[d.Key] = InstantStorage + ":" + strconv.Itoa(target) + "/" + file
		res.Disks = append(res.Disks, d.Key+" → "+newVol[d.Key]+" (reads from the backup)")
	}
	conf, cloudInit, notes := restoreConfig(g, newVol, opts.Name, clone)
	res.Notes = append(res.Notes, notes...)
	res.NewMACs = clone
	if opts.Name != "" {
		res.Name = opts.Name
	}
	conf = "description: BackupZit instant recovery from backup " + sn.ID.Short() + " of " + sn.Time.Local().Format("2006-01-02 15:04") +
		". Finish (move the disks to their storage) or discard it in the BackupZit console.\n" + stripDescription(conf)
	cfgFile := filepath.Join(ConfigDir, "nodes", inv.Node, "qemu-server", strconv.Itoa(target)+".conf")
	if err := os.WriteFile(cfgFile, []byte(conf), 0o640); err != nil {
		return nil, fmt.Errorf("create configuration: %w", err)
	}
	ok = true
	for key, st := range cloudInit {
		if _, err := command(ctx, "qm", "set", strconv.Itoa(target), "--"+key, st+":cloudinit"); err != nil {
			res.Notes = append(res.Notes, fmt.Sprintf("cloud-init drive %s not recreated: %v", key, err))
		}
	}
	if opts.Start {
		if _, err := command(ctx, "qm", "start", strconv.Itoa(target)); err != nil {
			res.Notes = append(res.Notes, "the VM could not be started: "+err.Error())
		} else {
			res.Started = true
		}
	}
	return res, nil
}

func stripDescription(conf string) string {
	var b strings.Builder
	for _, l := range strings.SplitAfter(conf, "\n") {
		if !strings.HasPrefix(l, "description:") && !strings.HasPrefix(l, "#") {
			b.WriteString(l)
		}
	}
	return b.String()
}

// InstantFinish moves the disks of an instant VM to storage while it runs
// and stops serving the backup.
func InstantFinish(ctx context.Context, vmid int, storage string, log func(string, ...any)) ([]string, error) {
	p, err := configPath("qemu", vmid)
	if err != nil {
		return nil, fmt.Errorf("guest %d: %w", vmid, err)
	}
	conf, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("guest %d: %w", vmid, err)
	}
	var notes []string
	moved, kept := 0, 0
	for _, line := range strings.Split(string(conf), "\n") {
		k, v, ok := strings.Cut(line, ":")
		v = strings.TrimSpace(v)
		if !ok || !qemuDiskKey.MatchString(strings.TrimSpace(k)) || !strings.HasPrefix(v, InstantStorage+":") {
			continue
		}
		key := strings.TrimSpace(k)
		vol, _, _ := strings.Cut(v, ",")
		if log != nil {
			log("moving disk", "vmid", vmid, "disk", key, "storage", storage)
		}
		_, err := command(ctx, "qm", "disk", "move", strconv.Itoa(vmid), key, storage, "--delete", "1")
		switch {
		case err == nil:
			moved++
		case strings.HasSuffix(vol, ".qcow2"):
			// This disk still reads from the backup: it must move.
			return notes, fmt.Errorf("move %s to %s: %w", key, storage, err)
		default:
			// Copied firmware variables or TPM state do not depend on the
			// backup; QEMU cannot move them while the VM runs.
			kept++
			notes = append(notes, fmt.Sprintf("%s stays on storage %s; to move it, shut the VM down and run: qm disk move %d %s %s --delete 1", key, InstantStorage, vmid, key, storage))
		}
	}
	if moved == 0 && kept == 0 {
		return nil, fmt.Errorf("guest %d has no disks running from a backup", vmid)
	}
	command(ctx, "systemctl", "stop", InstantUnit(vmid))
	os.Remove(InstantSocket(vmid))
	for _, f := range []string{".env", ".key"} {
		os.Remove(filepath.Join(InstantRunDir, strconv.Itoa(vmid)+f))
	}
	if kept == 0 {
		os.RemoveAll(filepath.Join(instantDir, "images", strconv.Itoa(vmid)))
	}
	return notes, nil
}

// InstantDiscard stops and deletes an instant VM.
func InstantDiscard(ctx context.Context, vmid int) error {
	p, err := configPath("qemu", vmid)
	if err != nil {
		return fmt.Errorf("guest %d: %w", vmid, err)
	}
	conf, err := os.ReadFile(p)
	if err != nil {
		return fmt.Errorf("guest %d: %w", vmid, err)
	}
	if !strings.Contains(string(conf), InstantStorage+":") {
		return fmt.Errorf("guest %d does not run from a backup", vmid)
	}
	command(ctx, "qm", "stop", strconv.Itoa(vmid))
	if _, err := command(ctx, "qm", "destroy", strconv.Itoa(vmid), "--purge", "1"); err != nil {
		return err
	}
	command(ctx, "systemctl", "stop", InstantUnit(vmid))
	os.Remove(InstantSocket(vmid))
	for _, f := range []string{".env", ".key"} {
		os.Remove(filepath.Join(InstantRunDir, strconv.Itoa(vmid)+f))
	}
	os.RemoveAll(filepath.Join(instantDir, "images", strconv.Itoa(vmid)))
	return nil
}

// efiVarsSize is the size of the OVMF variable store of a VM configuration.
func efiVarsSize(conf string) int64 {
	for _, l := range strings.Split(conf, "\n") {
		if strings.HasPrefix(l, "efidisk0:") && strings.Contains(l, "efitype=4m") {
			return 540672
		}
	}
	return 131072
}
