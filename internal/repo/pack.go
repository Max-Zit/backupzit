package repo

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
)

// Pack file layout:
//
//	blob_0 | blob_1 | ... | blob_n | header (JSON) | header length (uint32 LE)
//
// Each blob is stored either raw or zstd-compressed. The header lists every
// blob with its offset, so a pack can be fully understood without the index;
// the index is only an accelerator and can be rebuilt from pack headers.
// The pack's name is the SHA-256 of its whole content.

// PackedBlob describes one blob inside a pack.
type PackedBlob struct {
	Type   BlobType `json:"t"`
	ID     ID       `json:"id"`
	Offset uint32   `json:"o"`
	Length uint32   `json:"l"`           // stored length
	Raw    uint32   `json:"u,omitempty"` // uncompressed length; 0 = stored raw
}

const packTrailerLen = 4

// maxPackHeader bounds header reads from corrupt/hostile packs.
const maxPackHeader = 64 << 20

type packBuilder struct {
	buf   []byte
	blobs []PackedBlob
}

func (p *packBuilder) add(t BlobType, id ID, stored []byte, rawLen int) {
	p.blobs = append(p.blobs, PackedBlob{
		Type:   t,
		ID:     id,
		Offset: uint32(len(p.buf)),
		Length: uint32(len(stored)),
		Raw:    uint32(rawLen),
	})
	p.buf = append(p.buf, stored...)
}

func (p *packBuilder) size() int { return len(p.buf) }

// finish appends the (sealed) header and returns the complete pack and its ID.
func (p *packBuilder) finish(seal func([]byte) []byte) ([]byte, ID, []PackedBlob, error) {
	hdr, err := json.Marshal(p.blobs)
	if err != nil {
		return nil, ID{}, nil, err
	}
	hdr = seal(hdr)
	out := append(p.buf, hdr...)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(hdr)))
	return out, Hash(out), p.blobs, nil
}

func parsePackHeader(hdr []byte) ([]PackedBlob, error) {
	var blobs []PackedBlob
	if err := json.Unmarshal(hdr, &blobs); err != nil {
		return nil, fmt.Errorf("pack header: %w", err)
	}
	return blobs, nil
}

func packName(id ID) string {
	s := id.String()
	return "data/" + s[:2] + "/" + s
}
