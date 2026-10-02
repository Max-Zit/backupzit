package repo

// SystemLayout describes the disks of a Linux system backup, so the system
// can be recreated on an empty disk: partition tables, LVM, file systems
// (with their UUIDs and labels, so /etc/fstab and the boot loader keep
// working) and the boot code. The files themselves are in the snapshot tree.
type SystemLayout struct {
	OS       string       `json:"os"` // PRETTY_NAME
	Hostname string       `json:"hostname"`
	UEFI     bool         `json:"uefi"` // booted with UEFI
	Arch     string       `json:"arch"`
	Kernel   string       `json:"kernel,omitempty"`
	Disks    []SystemDisk `json:"disks"`
	// VGs holds LVM volume groups with their metadata backup (vgcfgbackup).
	VGs []SystemVG `json:"vgs,omitempty"`
	// FileSystems lists every backed up file system with how to recreate it.
	FileSystems []SystemFS `json:"filesystems"`
	// Swap lists swap partitions and volumes to recreate.
	Swap  []SystemFS `json:"swap,omitempty"`
	Fstab string     `json:"fstab,omitempty"`
}

// SystemDisk is a disk with its partition table.
type SystemDisk struct {
	Name       string `json:"name"` // sda, nvme0n1, vda
	Model      string `json:"model,omitempty"`
	Size       uint64 `json:"size"`
	SectorSize uint32 `json:"sector_size"`
	Table      string `json:"table"`        // gpt | dos
	ID         string `json:"id,omitempty"` // GPT disk GUID or MBR label id (0x...)
	// Head is a data blob with the first MiB of the disk: MBR boot code and
	// the area after it, where GRUB keeps its core image on BIOS systems.
	Head       ID                `json:"head"`
	Partitions []SystemPartition `json:"partitions"`
}

// SystemPartition is one partition of a SystemDisk.
type SystemPartition struct {
	Number int    `json:"number"`
	Start  uint64 `json:"start"`           // bytes
	Size   uint64 `json:"size"`            // bytes
	Type   string `json:"type"`            // GPT type GUID or MBR type (0x83)
	UUID   string `json:"uuid,omitempty"`  // GPT partition GUID
	Name   string `json:"name,omitempty"`  // GPT partition name
	Flags  string `json:"flags,omitempty"` // GPT attribute bits / "boot" for MBR
	// Content: "fs" (file system in FileSystems), "lvm" (physical volume),
	// "swap", "bios_grub", "raw" (copied as blocks in Raw), "" (ignored).
	Content string `json:"content"`
	// Raw holds the partition's blocks for small partitions without a
	// backed up file system (BIOS boot partition).
	Raw []ID `json:"raw,omitempty"`
	// PVUUID is the LVM physical volume UUID.
	PVUUID string `json:"pv_uuid,omitempty"`
	VG     string `json:"vg,omitempty"`
}

// SystemVG is an LVM volume group.
type SystemVG struct {
	Name   string `json:"name"`
	Config string `json:"config"` // vgcfgbackup output
}

// SystemFS is a file system (or swap area) to recreate.
type SystemFS struct {
	Device     string `json:"device"` // partition "sda2" or LV "vg/lv"
	Type       string `json:"type"`   // ext4, xfs, vfat, swap, ...
	UUID       string `json:"uuid"`
	Label      string `json:"label,omitempty"`
	MountPoint string `json:"mount_point,omitempty"`
	Size       uint64 `json:"size"`
	Used       uint64 `json:"used,omitempty"`
	// Options for mkfs to recreate the same on-disk format, e.g. ext4
	// features ("-O none,has_journal,...") or vfat FAT size.
	MkfsArgs []string `json:"mkfs_args,omitempty"`
}
