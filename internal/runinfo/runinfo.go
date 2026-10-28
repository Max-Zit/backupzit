// Package runinfo builds the kind specific details of a backup run from its
// snapshot. Agents report them when a backup finishes; the console builds
// the same for backups it finds on storage.
package runinfo

import (
	"encoding/json"

	"github.com/max-zit/backupzit/internal/repo"
)

// Image is the disk layout of an image backup: one disk as an object (as
// before several disks were possible), several as a list.
func Image(sn *repo.Snapshot) json.RawMessage {
	var b []byte
	if len(sn.Images) == 1 {
		b, _ = json.Marshal(sn.Images[0])
	} else {
		b, _ = json.Marshal(sn.Images)
	}
	return b
}

// GuestSummary describes a virtual machine or container in a VM backup.
type GuestSummary struct {
	VMID        int    `json:"vmid"`
	Type        string `json:"type"`
	Name        string `json:"name"`
	Consistency string `json:"consistency"`
	Platform    string `json:"platform,omitempty"`
	Disks       int    `json:"disks"`
	Size        uint64 `json:"size"`
	Stored      uint64 `json:"stored"` // bytes with data
	// Changed is the number of disks of which only the blocks changed
	// since the previous backup were read (changed block tracking).
	Changed int `json:"changed,omitempty"`
}

// Guests lists the guests of a VM backup.
func Guests(sn *repo.Snapshot) json.RawMessage {
	var sum []GuestSummary
	for _, g := range sn.Guests {
		s := GuestSummary{VMID: g.VMID, Type: g.Type, Name: g.Name, Consistency: g.Consistency, Disks: len(g.Disks), Platform: g.Platform}
		for _, d := range g.Disks {
			s.Size += d.Size
			if d.Image < len(sn.Images) && len(sn.Images[d.Image].Partitions) == 1 {
				p := sn.Images[d.Image].Partitions[0]
				s.Stored += p.StoredBytes
				if p.Method == "changed-blocks" {
					s.Changed++
				}
			}
		}
		sum = append(sum, s)
	}
	b, _ := json.Marshal(map[string]any{"guests": sum})
	return b
}

// SystemSummary describes the machine of a Linux system backup.
type SystemSummary struct {
	OS          string          `json:"os"`
	UEFI        bool            `json:"uefi"`
	Disks       []SystemDisk    `json:"disks"`
	FileSystems []repo.SystemFS `json:"file_systems"`
	VGs         []string        `json:"vgs,omitempty"`
}

// SystemDisk is a disk of a Linux system backup.
type SystemDisk struct {
	Name  string `json:"name"`
	Size  uint64 `json:"size"`
	Table string `json:"table"`
	Parts int    `json:"partitions"`
}

// System summarizes the disk layout of a Linux system backup.
func System(sn *repo.Snapshot) json.RawMessage {
	lay := sn.System
	if lay == nil {
		return nil
	}
	sum := SystemSummary{OS: lay.OS, UEFI: lay.UEFI, FileSystems: lay.FileSystems}
	for _, d := range lay.Disks {
		sum.Disks = append(sum.Disks, SystemDisk{Name: d.Name, Size: d.Size, Table: d.Table, Parts: len(d.Partitions)})
	}
	for _, v := range lay.VGs {
		sum.VGs = append(sum.VGs, v.Name)
	}
	b, _ := json.Marshal(map[string]any{"system": sum})
	return b
}

// SQL lists the databases of a database backup.
func SQL(sn *repo.Snapshot) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"sql": sn.SQL})
	return b
}
