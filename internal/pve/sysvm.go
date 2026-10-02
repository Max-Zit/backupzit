package pve

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// SystemVMOptions describe a new VM for restoring a physical or virtual
// machine into Proxmox VE (P2V / V2V).
type SystemVMOptions struct {
	VMID     int // 0 or negative: next free ID
	Name     string
	Storage  string
	DiskSize uint64 // bytes, at least the size of the original disk
	UEFI     bool
	Memory   int // MiB
	Cores    int
	Bridge   string
	Windows  bool
}

// CreateSystemVM creates a stopped VM with one empty VirtIO SCSI disk and
// returns its ID and the disk's device or file path.
func CreateSystemVM(ctx context.Context, o SystemVMOptions) (int, string, error) {
	if o.VMID <= 0 {
		id, err := NextID(ctx)
		if err != nil {
			return 0, "", err
		}
		o.VMID = id
	}
	if o.Memory <= 0 {
		o.Memory = 2048
	}
	if o.Cores <= 0 {
		o.Cores = 2
	}
	if o.Bridge == "" {
		o.Bridge = "vmbr0"
	}
	if o.Name == "" {
		o.Name = fmt.Sprintf("restored-%d", o.VMID)
	}
	stores, err := readStorage()
	if err != nil {
		return 0, "", err
	}
	st, ok := stores[o.Storage]
	if !ok || !st.usable(LocalNode()) || !st.has("images") {
		return 0, "", fmt.Errorf("storage %q cannot hold VM disks on this node", o.Storage)
	}
	// Round up with some room for the backup GPT and alignment.
	gib := (o.DiskSize + 64<<20 + (1<<30 - 1)) >> 30
	if gib == 0 {
		gib = 1
	}
	ostype := "l26"
	if o.Windows {
		ostype = "win11"
	}
	args := []string{"create", strconv.Itoa(o.VMID), "--name", o.Name, "--memory", strconv.Itoa(o.Memory), "--cores", strconv.Itoa(o.Cores),
		"--ostype", ostype, "--scsihw", "virtio-scsi-single", "--net0", "virtio,bridge=" + o.Bridge,
		"--scsi0", fmt.Sprintf("%s:%d", o.Storage, gib), "--boot", "order=scsi0", "--agent", "enabled=1",
		"--description", "Restored by BackupZit"}
	if o.UEFI {
		args = append(args, "--bios", "ovmf", "--machine", "q35", "--efidisk0", o.Storage+":1,efitype=4m,pre-enrolled-keys=0")
	}
	if _, err := command(ctx, "qm", args...); err != nil {
		return 0, "", err
	}
	cfg, err := command(ctx, "qm", "config", strconv.Itoa(o.VMID))
	if err != nil {
		return o.VMID, "", err
	}
	m := regexp.MustCompile(`(?m)^scsi0: ([^,\s]+)`).FindSubmatch(cfg)
	if m == nil {
		return o.VMID, "", fmt.Errorf("new VM %d has no disk", o.VMID)
	}
	path, err := volumePath(ctx, string(m[1]))
	if err != nil {
		return o.VMID, "", err
	}
	if st.Type == "lvm" || st.Type == "lvmthin" {
		_, vol, _ := strings.Cut(string(m[1]), ":")
		command(ctx, "lvchange", "-ay", st.Props["vgname"]+"/"+vol)
	}
	return o.VMID, path, nil
}

// DestroyVM removes a VM created for a restore that failed.
func DestroyVM(ctx context.Context, vmid int) {
	command(ctx, "qm", "destroy", strconv.Itoa(vmid), "--purge")
}

// StartVM starts a VM.
func StartVM(ctx context.Context, vmid int) error {
	_, err := command(ctx, "qm", "start", strconv.Itoa(vmid))
	return err
}
