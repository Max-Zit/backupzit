package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Guest is a virtual machine or container backed up from a hypervisor
// without an agent inside it. Its disks are stored as DiskImages of the
// snapshot (one whole-disk partition each), so retention, prune, copy and
// restore tests treat them like any other image. Container volumes that are
// file systems rather than block devices (Proxmox ZFS subvolumes) are stored
// as directories of the snapshot's tree instead, see GuestDisk.Path.
type Guest struct {
	Platform string `json:"platform"` // "proxmox", "hyperv" or "vmware"
	Type     string `json:"type"`     // "qemu" (VM) or "lxc" (container)
	VMID     int    `json:"vmid"`
	// ID is the Hyper-V VM GUID or VMware instance UUID (empty for Proxmox).
	ID      string `json:"id,omitempty"`
	Name    string `json:"name,omitempty"`
	Node    string `json:"node,omitempty"`
	Running bool   `json:"running,omitempty"`
	// Consistency describes how the disks were captured, e.g.
	// "snapshot, file systems frozen by the guest agent".
	Consistency string `json:"consistency,omitempty"`
	// Config is the guest configuration as stored by the hypervisor.
	Config string      `json:"config"`
	Disks  []GuestDisk `json:"disks"`
	// ExcludedDisks are disks of the guest left out of the backup by the
	// job (e.g. data disks backed up another way): not restored.
	ExcludedDisks []ExcludedDisk `json:"excluded_disks,omitempty"`
}

// StateBlobs returns additional data blobs referenced by the guest's
// configuration, rather than by its disk images. Keep this reader compatible
// with existing snapshots: Hyper-V 0.48 stores its VMGS ID inside Config.
// Invalid references must not be ignored, or maintenance could discard state
// needed to restore a retained guest.
func (g Guest) StateBlobs() ([]ID, error) {
	if g.Platform != "hyperv" || g.Config == "" {
		return nil, nil
	}
	var cfg struct {
		GuestState string `json:"gueststate"`
	}
	if err := json.Unmarshal([]byte(g.Config), &cfg); err != nil {
		return nil, fmt.Errorf("read Hyper-V guest state reference: %w", err)
	}
	if cfg.GuestState == "" {
		return nil, nil // older backups, or a VM whose state was not saved
	}
	id, err := ParseID(cfg.GuestState)
	if err != nil {
		return nil, fmt.Errorf("invalid Hyper-V guest state blob: %w", err)
	}
	if id.IsNull() {
		return nil, fmt.Errorf("invalid Hyper-V guest state blob: null ID")
	}
	return []ID{id}, nil
}

// GuestDisk is one disk (or container volume) of a guest.
type GuestDisk struct {
	Key     string `json:"key"`    // configuration key: scsi0, efidisk0, rootfs, mp0
	Volume  string `json:"volume"` // volume ID at backup time, storage:name
	Storage string `json:"storage"`
	Format  string `json:"format,omitempty"` // raw, qcow2, subvol, ...
	// Size is the size of the disk; for a subvolume its quota (0: none).
	Size uint64 `json:"size"`
	// Image is the index of the disk's data in Snapshot.Images; -1 for a
	// volume stored as files (Path).
	Image int `json:"image"`
	// Path is set for a volume stored as files: the directory in the
	// snapshot's tree that holds its contents ("lxc/101/rootfs"). Being part
	// of the tree, its data is kept, checked and copied like that of a file
	// backup.
	Path string `json:"path,omitempty"`
	// Bytes is the total size of the files of a volume stored as files.
	Bytes uint64 `json:"bytes,omitempty"`
	// ChangeID is the changed block tracking position of the disk at the
	// backup (VMware change ID, Hyper-V RCT, Proxmox QEMU dirty bitmap);
	// the next backup reads only what changed since.
	ChangeID string `json:"change_id,omitempty"`
	// Via is how a running VM's disk was read when not the default: "hot-add"
	// (VMware: attached to the proxy agent's VM instead of copied).
	Via string `json:"via,omitempty"`
}

// ExcludedDisk is a disk a job left out of a VM backup.
type ExcludedDisk struct {
	Key  string `json:"key"`  // controller location, e.g. scsi0:1
	Path string `json:"path"` // disk file on the host
}

// Files reports whether the volume is stored as files rather than as a disk
// image.
func (d GuestDisk) Files() bool { return d.Path != "" }

// FindNode returns the node at path ("a/b/c") below the tree root.
func (r *Repository) FindNode(ctx context.Context, root ID, path string) (*Node, error) {
	if path == "" {
		return nil, errors.New("empty path")
	}
	tree := root
	comps := strings.Split(path, "/")
	for i, c := range comps {
		t, err := r.LoadTree(ctx, tree)
		if err != nil {
			return nil, err
		}
		n := t.Find(c)
		if n == nil {
			return nil, fmt.Errorf("%s: not in the snapshot", strings.Join(comps[:i+1], "/"))
		}
		if i == len(comps)-1 {
			return n, nil
		}
		if n.Type != NodeDir || n.Subtree == nil {
			return nil, fmt.Errorf("%s: not a directory", strings.Join(comps[:i+1], "/"))
		}
		tree = *n.Subtree
	}
	return nil, errors.New("unreachable")
}
