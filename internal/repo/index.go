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
	// recovered are packs that came from their own headers because an index
	// file was damaged. Their blobs can be read, but are not reused by new
	// backups (see Reusable) until RebuildIndex has verified the pack.
	recovered map[ID]struct{}
}

func newIndex() *Index {
	return &Index{blobs: map[BlobHandle]Location{}, packs: map[ID]struct{}{}, recovered: map[ID]struct{}{}}
}

// addRecoveredPack is addPack for a pack read from its header.
func (x *Index) addRecoveredPack(pack ID, blobs []PackedBlob) {
	x.mu.Lock()
	x.recovered[pack] = struct{}{}
	x.mu.Unlock()
	x.addPack(pack, blobs)
}

// RecoveredPacks returns the packs that were read from their headers and are
// not verified yet.
func (x *Index) RecoveredPacks() []ID {
	x.mu.RLock()
	defer x.mu.RUnlock()
	out := make([]ID, 0, len(x.recovered))
	for p := range x.recovered {
		out = append(out, p)
	}
	return out
}

// Reusable reports whether a blob is indexed in a pack that a new backup may
// deduplicate against: not in a pack taken from its header only. A pack
// that repair dropped as damaged can come back that way (its header is
// intact, its data is not), and reusing it would keep the lost data.
func (x *Index) Reusable(h BlobHandle) bool {
	x.mu.RLock()
	defer x.mu.RUnlock()
	l, ok := x.blobs[h]
	if !ok {
		return false
	}
	_, rec := x.recovered[l.Pack]
	return !rec
}

func (x *Index) addPack(pack ID, blobs []PackedBlob) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.packs[pack] = struct{}{}
	for _, b := range blobs {
		h := BlobHandle{Type: b.Type, ID: b.ID}
		if old, ok := x.blobs[h]; ok {
			// Duplicate copy in another pack; first one wins, except that a
			// copy in a recovered (unverified) pack gives way to a copy in
			// a pack the index lists: a backup that stored the data again.
			_, oldRec := x.recovered[old.Pack]
			_, newRec := x.recovered[pack]
			if !oldRec || newRec {
				continue
			}
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

// StoredBytes is the size of the indexed data as stored (compressed and
// encrypted), i.e. roughly the space the repository takes on the storage.
func (x *Index) StoredBytes() uint64 {
	x.mu.RLock()
	defer x.mu.RUnlock()
	var n uint64
	for _, l := range x.blobs {
		n += uint64(l.Length)
	}
	return n
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
