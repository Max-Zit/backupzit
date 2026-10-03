package repo

// Guest is a virtual machine or container backed up from a hypervisor
// without an agent inside it. Its disks are stored as DiskImages of the
// snapshot (one whole-disk partition each), so retention, prune, copy and
// restore tests treat them like any other image.
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
}

// GuestDisk is one disk (or container volume) of a guest.
type GuestDisk struct {
	Key     string `json:"key"`    // configuration key: scsi0, efidisk0, rootfs, mp0
	Volume  string `json:"volume"` // volume ID at backup time, storage:name
	Storage string `json:"storage"`
	Format  string `json:"format,omitempty"` // raw, qcow2, ...
	Size    uint64 `json:"size"`
	// Image is the index of the disk's data in Snapshot.Images.
	Image int `json:"image"`
	// ChangeID is the changed block tracking position of the disk at the
	// backup (VMware); the next backup reads only what changed since.
	ChangeID string `json:"change_id,omitempty"`
}
