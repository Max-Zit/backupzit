package vmware

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// sysfs is the root of /sys (tests point it at a fake tree).
var sysfs = "/sys"

// ownVMUUIDs returns the BIOS UUID of the machine the agent runs on, as
// vSphere knows it (config.uuid), in both byte orders: older kernels show
// the first three fields of VMware's SMBIOS UUID byte-swapped.
var ownVMUUIDs = func() ([]string, error) {
	b, err := os.ReadFile(filepath.Join(sysfs, "class/dmi/id/product_uuid"))
	if err != nil {
		return nil, fmt.Errorf("hot-add: the agent's machine has no readable BIOS UUID (%v); it needs a Linux agent in a VM, running as root", err)
	}
	id := strings.ToLower(strings.TrimSpace(string(b)))
	if len(id) != 36 {
		return nil, fmt.Errorf("hot-add: unexpected BIOS UUID %q", id)
	}
	return []string{id, swapUUID(id)}, nil
}

// blockDevices lists the whole disks the kernel knows, with their size in
// bytes.
var blockDevices = func() (map[string]uint64, error) {
	ents, err := os.ReadDir(filepath.Join(sysfs, "block"))
	if err != nil {
		return nil, err
	}
	out := map[string]uint64{}
	for _, e := range ents {
		n := e.Name()
		if !strings.HasPrefix(n, "sd") && !strings.HasPrefix(n, "vd") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(sysfs, "block", n, "size")) // #nosec G304 -- sysfs attribute of a disk the kernel lists
		if err != nil {
			continue
		}
		sectors, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
		if err != nil {
			continue
		}
		out[n] = sectors * 512
	}
	return out, nil
}

// rescanSCSI asks every SCSI host to look for new disks (hot-added disks
// usually appear by themselves; this covers controllers that wait).
func rescanSCSI() {
	hosts, _ := filepath.Glob(filepath.Join(sysfs, "class/scsi_host/host*/scan"))
	for _, h := range hosts {
		_ = os.WriteFile(h, []byte("- - -"), 0o200) // #nosec G306 -- sysfs control file
	}
}

// diskWait bounds how long an attached disk may take to appear.
var diskWait = 2 * time.Minute

// waitForDisk waits for the one new disk of the given size. Attaching is
// serialized by lockHotAdd, so a new disk of that size is the one just
// attached.
var waitForDisk = func(ctx context.Context, before map[string]uint64, size uint64) (string, error) {
	deadline := time.Now().Add(diskWait)
	for i := 0; ; i++ {
		now, err := blockDevices()
		if err != nil {
			return "", err
		}
		var found []string
		for n, s := range now {
			if _, old := before[n]; !old && s == size {
				found = append(found, n)
			}
		}
		switch {
		case len(found) == 1:
			return "/dev/" + found[0], nil
		case len(found) > 1:
			return "", fmt.Errorf("hot-add: several new disks of %d bytes appeared (%s); not reading any", size, strings.Join(found, ", "))
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("hot-add: the attached disk did not appear in the agent's VM within %s", diskWait)
		}
		if i%5 == 4 {
			rescanSCSI()
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// releaseDisk undoes what the proxy's system may have done with the disk
// (LVM or device-mapper volumes activated on it by udev) and removes the
// device from the kernel before vSphere detaches it.
var releaseDisk = func(dev string, log func(string, ...any)) {
	name := filepath.Base(dev)
	holders, _ := filepath.Glob(filepath.Join(sysfs, "block", name, "holders", "*"))
	parts, _ := filepath.Glob(filepath.Join(sysfs, "block", name, name+"*", "holders", "*"))
	for _, h := range append(holders, parts...) {
		dm, err := os.ReadFile(filepath.Join(sysfs, "block", filepath.Base(h), "dm", "name"))
		if err != nil {
			continue
		}
		n := strings.TrimSpace(string(dm))
		log("hot-add: removing a volume the agent's system activated on the attached disk", "volume", n)
		if out, err := exec.Command("dmsetup", "remove", "--retry", n).CombinedOutput(); err != nil { // #nosec G204 -- device-mapper name from sysfs
			log("hot-add: dmsetup remove failed", "volume", n, "error", strings.TrimSpace(string(out)))
		}
	}
	_ = os.WriteFile(filepath.Join(sysfs, "block", name, "device", "delete"), []byte("1"), 0o200) // #nosec G306 -- sysfs control file
}

// lockHotAdd serializes attaching disks and finding their devices across
// the agent's runs (several backups may run at once).
var lockHotAdd = func() (func(), error) {
	f, err := os.OpenFile("/run/backupzit-hotadd.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil { // #nosec G115 -- file descriptor
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) // #nosec G115 -- file descriptor
		_ = f.Close()
	}, nil
}
