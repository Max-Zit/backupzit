package imaging

import (
	"fmt"
	"strings"
)

// DiskIdentity tells a disk apart from the others while disk numbers can
// change: a USB disk that is plugged in again gets the next free number,
// and another disk (for example the one holding the backups) may then get
// the number the user chose. A restore takes the identity of the disk it
// was confirmed for and checks it again on the opened device before it
// writes, and before each later step.
type DiskIdentity struct {
	Size         uint64 `json:"size"`
	SectorSize   uint32 `json:"sector_size"`
	Model        string `json:"model,omitempty"`
	Serial       string `json:"serial,omitempty"`
	Bus          string `json:"bus,omitempty"`
	Style        string `json:"style"`
	GPTDiskID    string `json:"gpt_disk_id,omitempty"`
	MBRSignature uint32 `json:"mbr_signature,omitempty"`
}

// Identity returns the identity of d as listed now.
func (d Disk) Identity() DiskIdentity {
	return DiskIdentity{Size: d.Size, SectorSize: d.SectorSize, Model: strings.TrimSpace(d.Model), Serial: strings.TrimSpace(d.Serial),
		Bus: d.Bus, Style: d.Style, GPTDiskID: d.GPTDiskID, MBRSignature: d.MBRSignature}
}

// WithTable is the identity after a new partition table was written: the
// same device with the table's style and identifiers.
func (id DiskIdentity) WithTable(style, gptDiskID string, mbrSignature uint32) DiskIdentity {
	id.Style, id.GPTDiskID, id.MBRSignature = style, "", 0
	switch style {
	case StyleGPT:
		id.GPTDiskID = gptDiskID
	case StyleMBR:
		id.MBRSignature = mbrSignature
	}
	return id
}

// Matches returns nil when d is the disk id describes, otherwise an error
// that names what differs.
func (id DiskIdentity) Matches(d Disk) error {
	var diff []string
	if d.Size != id.Size {
		diff = append(diff, fmt.Sprintf("size %d bytes instead of %d", d.Size, id.Size))
	}
	if d.SectorSize != id.SectorSize {
		diff = append(diff, fmt.Sprintf("%d byte sectors instead of %d", d.SectorSize, id.SectorSize))
	}
	if m := strings.TrimSpace(d.Model); m != id.Model {
		diff = append(diff, fmt.Sprintf("model %q instead of %q", m, id.Model))
	}
	if s := strings.TrimSpace(d.Serial); s != id.Serial {
		diff = append(diff, fmt.Sprintf("serial number %q instead of %q", s, id.Serial))
	}
	if d.Bus != id.Bus {
		diff = append(diff, fmt.Sprintf("bus %q instead of %q", d.Bus, id.Bus))
	}
	if d.Style != id.Style {
		diff = append(diff, fmt.Sprintf("partition table %s instead of %s", strings.ToUpper(d.Style), strings.ToUpper(id.Style)))
	} else {
		switch id.Style {
		case StyleGPT:
			if !strings.EqualFold(d.GPTDiskID, id.GPTDiskID) {
				diff = append(diff, fmt.Sprintf("GPT disk ID %s instead of %s", d.GPTDiskID, id.GPTDiskID))
			}
		case StyleMBR:
			if d.MBRSignature != id.MBRSignature {
				diff = append(diff, fmt.Sprintf("disk signature %08X instead of %08X", d.MBRSignature, id.MBRSignature))
			}
		}
	}
	if len(diff) > 0 {
		return fmt.Errorf("disk %d is no longer the disk that was chosen (%s); nothing was written to it: read the disks again and choose the target again", d.Number, strings.Join(diff, ", "))
	}
	return nil
}

// DiskRef names the disk a restore step works on: its number and the
// identity it must still have. Without an identity the number alone
// counts, as for runs the console checked itself.
type DiskRef struct {
	Number int
	ID     *DiskIdentity
}

// Pick returns the disk ref names from disks, or an error when no disk
// has its number or the disk with its number is another one.
func (r DiskRef) Pick(disks []Disk) (*Disk, error) {
	for i := range disks {
		if disks[i].Number != r.Number {
			continue
		}
		if r.ID != nil {
			if err := r.ID.Matches(disks[i]); err != nil {
				return nil, err
			}
		}
		return &disks[i], nil
	}
	return nil, fmt.Errorf("disk %d not found", r.Number)
}

// DiskOfPath returns the number of the disk that holds path: a drive
// letter path (E:\backups), a \\?\ path, a volume GUID path
// (\\?\Volume{…}\) or a path below a folder mount point. The deepest mount
// point that contains the path wins.
func DiskOfPath(disks []Disk, path string) (int, bool) {
	p := strings.ToLower(strings.ReplaceAll(path, "/", `\`))
	if strings.HasPrefix(p, `\\?\`) && !strings.HasPrefix(p, `\\?\volume{`) {
		p = p[len(`\\?\`):]
	}
	best, bestLen := -1, 0
	consider := func(n int, root string) {
		r := strings.ToLower(strings.TrimRight(strings.ReplaceAll(root, "/", `\`), `\`))
		if r == "" {
			return
		}
		if (p == r || strings.HasPrefix(p, r+`\`)) && len(r) > bestLen {
			best, bestLen = n, len(r)
		}
	}
	for _, d := range disks {
		for _, part := range d.Partitions {
			for _, m := range part.MountPoints {
				consider(d.Number, m)
			}
			consider(d.Number, part.VolumeGUIDPath)
		}
	}
	return best, best >= 0
}
