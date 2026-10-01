package repo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// ID is the SHA-256 of a blob's plaintext (or of a file's full content for
// packs, indexes and snapshots).
type ID [32]byte

// Hash returns the ID of data.
func Hash(data []byte) ID { return sha256.Sum256(data) }

func (id ID) String() string { return hex.EncodeToString(id[:]) }

// Short returns the first 8 hex characters, used for display.
func (id ID) Short() string { return id.String()[:8] }

func (id ID) IsNull() bool { return id == ID{} }

// ParseID parses a 64 character hex string.
func ParseID(s string) (ID, error) {
	var id ID
	b, err := hex.DecodeString(s)
	if err != nil {
		return id, fmt.Errorf("invalid id %q: %w", s, err)
	}
	if len(b) != len(id) {
		return id, fmt.Errorf("invalid id %q: wrong length", s)
	}
	copy(id[:], b)
	return id, nil
}

func (id ID) MarshalJSON() ([]byte, error) { return json.Marshal(id.String()) }

func (id *ID) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	p, err := ParseID(s)
	if err != nil {
		return err
	}
	*id = p
	return nil
}

// BlobType distinguishes file content from directory metadata.
type BlobType uint8

const (
	DataBlob BlobType = 1
	TreeBlob BlobType = 2
)

func (t BlobType) String() string {
	switch t {
	case DataBlob:
		return "data"
	case TreeBlob:
		return "tree"
	case MapBlob:
		return "map"
	}
	return fmt.Sprintf("blobtype(%d)", uint8(t))
}

// BlobHandle identifies a blob within the repository.
type BlobHandle struct {
	Type BlobType
	ID   ID
}
