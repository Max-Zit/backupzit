package repo

import (
	"encoding/json"
	"sync"
)

// Location says where a blob is stored.
type Location struct {
	Pack   ID
	Offset uint32
	Length uint32
	Raw    uint32
}

// indexFile is the on-disk format of index/<id>.
type indexFile struct {
	Packs []indexPack `json:"packs"`
}

type indexPack struct {
	ID    ID           `json:"id"`
	Blobs []PackedBlob `json:"blobs"`
}

// Index maps blobs to their location. It is safe for concurrent use.
type Index struct {
	mu    sync.RWMutex
	blobs map[BlobHandle]Location
	packs map[ID]struct{}
}

func newIndex() *Index {
	return &Index{blobs: map[BlobHandle]Location{}, packs: map[ID]struct{}{}}
}

func (x *Index) addPack(pack ID, blobs []PackedBlob) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.packs[pack] = struct{}{}
	for _, b := range blobs {
		h := BlobHandle{Type: b.Type, ID: b.ID}
		if _, ok := x.blobs[h]; ok {
			continue // duplicate copy in another pack; first one wins
		}
		x.blobs[h] = Location{Pack: pack, Offset: b.Offset, Length: b.Length, Raw: b.Raw}
	}
}

// Lookup returns the location of a blob.
func (x *Index) Lookup(h BlobHandle) (Location, bool) {
	x.mu.RLock()
	defer x.mu.RUnlock()
	l, ok := x.blobs[h]
	return l, ok
}

// Has reports whether a blob is indexed.
func (x *Index) Has(h BlobHandle) bool {
	_, ok := x.Lookup(h)
	return ok
}

// Len returns the number of indexed blobs.
func (x *Index) Len() int {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return len(x.blobs)
}

// Packs returns the set of packs referenced by the index.
func (x *Index) Packs() map[ID]struct{} {
	x.mu.RLock()
	defer x.mu.RUnlock()
	out := make(map[ID]struct{}, len(x.packs))
	for k := range x.packs {
		out[k] = struct{}{}
	}
	return out
}

func encodeIndexFile(packs []indexPack) ([]byte, error) {
	return json.Marshal(indexFile{Packs: packs})
}

func decodeIndexFile(b []byte) (indexFile, error) {
	var f indexFile
	err := json.Unmarshal(b, &f)
	return f, err
}
