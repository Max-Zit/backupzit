package repo

import (
	"context"
	"fmt"
)

// MapBlob holds a block map of a partition image: one 32 byte blob ID per
// block, where the null ID means "not stored" (unused file system space).
const MapBlob BlobType = 3

// mapEntriesPerBlob bounds map blob size to 256 KiB.
const mapEntriesPerBlob = 8192

// DiskImage records the layout of a backed up disk and its partition images.
type DiskImage struct {
	Number       int    `json:"number"`
	Model        string `json:"model,omitempty"`
	Size         uint64 `json:"size"`
	SectorSize   uint32 `json:"sector_size"`
	Style        string `json:"style"` // "gpt" | "mbr"
	GPTDiskID    string `json:"gpt_disk_id,omitempty"`
	MBRSignature uint32 `json:"mbr_signature,omitempty"`
	// Head is a data blob with the first MiB of the disk (MBR boot code,
	// GPT header and entries), used to restore boot code on MBR disks.
	Head       ID               `json:"head"`
	Partitions []PartitionImage `json:"partitions"`
}

// PartitionImage records one partition and, if included, its contents.
type PartitionImage struct {
	Number        int      `json:"number"`
	Offset        uint64   `json:"offset"`
	Length        uint64   `json:"length"`
	GPTType       string   `json:"gpt_type,omitempty"`
	GPTID         string   `json:"gpt_id,omitempty"`
	GPTAttributes uint64   `json:"gpt_attributes,omitempty"`
	Name          string   `json:"name,omitempty"`
	MBRType       uint8    `json:"mbr_type,omitempty"`
	Bootable      bool     `json:"bootable,omitempty"`
	MountPoints   []string `json:"mount_points,omitempty"`
	FileSystem    string   `json:"file_system,omitempty"`
	Label         string   `json:"label,omitempty"`

	// Included is false for partitions that are part of the layout but
	// whose contents were not backed up.
	Included bool `json:"included"`
	// Method is "used-blocks" (only allocated file system clusters) or "full".
	Method string `json:"method,omitempty"`
	// Source is "vss" when read from a shadow copy, otherwise "live".
	Source    string `json:"source,omitempty"`
	BlockSize uint32 `json:"block_size,omitempty"`
	Blocks    uint64 `json:"blocks,omitempty"`
	// Maps are MapBlobs; concatenated they hold one ID per block.
	Maps []ID `json:"maps,omitempty"`
	// StoredBytes is the size of all blocks that carry data.
	StoredBytes uint64 `json:"stored_bytes,omitempty"`
}

// SaveBlockMap stores the block IDs of a partition as map blobs.
func (r *Repository) SaveBlockMap(ctx context.Context, ids []ID) ([]ID, error) {
	var maps []ID
	for start := 0; start < len(ids); start += mapEntriesPerBlob {
		end := min(start+mapEntriesPerBlob, len(ids))
		buf := make([]byte, 0, (end-start)*len(ID{}))
		for _, id := range ids[start:end] {
			buf = append(buf, id[:]...)
		}
		id, _, err := r.SaveBlob(ctx, MapBlob, buf)
		if err != nil {
			return nil, err
		}
		maps = append(maps, id)
	}
	return maps, nil
}

// LoadBlockMap returns the block IDs of a partition image.
func (r *Repository) LoadBlockMap(ctx context.Context, p *PartitionImage) ([]ID, error) {
	ids := make([]ID, 0, p.Blocks)
	for _, m := range p.Maps {
		b, err := r.LoadBlob(ctx, MapBlob, m)
		if err != nil {
			return nil, fmt.Errorf("block map %s: %w", m.Short(), err)
		}
		if len(b)%len(ID{}) != 0 {
			return nil, fmt.Errorf("block map %s: bad length %d", m.Short(), len(b))
		}
		for i := 0; i < len(b); i += len(ID{}) {
			var id ID
			copy(id[:], b[i:])
			ids = append(ids, id)
		}
	}
	if uint64(len(ids)) != p.Blocks {
		return nil, fmt.Errorf("block map has %d entries, partition has %d blocks", len(ids), p.Blocks)
	}
	return ids, nil
}
