package repo

import (
	"context"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/max-zit/backupzit/internal/backend"
	"github.com/restic/chunker"
)

// FormatVersion is the repository format this code writes.
const FormatVersion = 1

// DefaultPackSize is the target size of a pack file.
const DefaultPackSize = 16 << 20

// indexFlushPacks writes an intermediate index after this many packs so an
// interrupted backup does not leave too much unindexed data behind.
const indexFlushPacks = 32

// Config is stored unencrypted at "config". For encrypted repositories the
// chunker polynomial is kept in the key file instead.
type Config struct {
	Version           int       `json:"version"`
	ID                string    `json:"id"`
	Created           time.Time `json:"created"`
	ChunkerPolynomial string    `json:"chunker_polynomial,omitempty"`
	PackSize          int       `json:"pack_size"`
	Encryption        string    `json:"encryption,omitempty"`
}

// ErrNotInitialized means no config exists at the location.
var ErrNotInitialized = errors.New("repository not initialized")

// Repository stores deduplicated, compressed blobs in pack files.
type Repository struct {
	be  backend.Backend
	cfg Config
	pol chunker.Pol
	idx *Index
	// indexFiles are the index files in idx (refreshIndex loads only those
	// written since, or everything again when one is gone: a prune).
	indexMu    sync.Mutex
	indexFiles map[string]bool

	enc *zstd.Encoder
	// encLevel is the level of enc (zero means zstd.SpeedDefault); a
	// BlobWriter builds its own encoder with the same level so that its
	// output is byte-identical.
	encLevel zstd.EncoderLevel
	dec      *zstd.Decoder
	// aead encrypts all stored data; nil for unencrypted repositories.
	aead cipher.AEAD

	mu             sync.Mutex
	pack           *packBuilder
	pending        map[BlobHandle]struct{}
	unindexed      []indexPack // packs uploaded since the last index write
	stats          WriteStats
	noCompress     bool // store new blobs uncompressed (SetCompression("off"))
	vmBlockSizeKiB int  // session option; zero keeps the historical 1 MiB

	// Packs are uploaded in the background, up to maxUploads at once, on
	// backends that handle parallel uploads (object stores); elsewhere
	// maxUploads is 1 and the caller uploads the pack as before. The blobs
	// of a pack in flight stay in pending until it is uploaded, so the
	// index only ever names packs that exist. upDone (on mu) is signalled
	// when an upload ends; upErr keeps the first failure.
	maxUploads int
	inflight   int
	upDone     *sync.Cond
	upErr      error
}

// WriteStats counts what this session wrote to the backend.
type WriteStats struct {
	NewBlobs      int
	DupBlobs      int
	RawBytes      uint64 // uncompressed size of new blobs
	StoredBytes   uint64 // bytes uploaded in packs (excluding headers)
	PacksUploaded int
}

// Init creates a new repository at the backend location. With Password
// the repository is encrypted.
func Init(ctx context.Context, be backend.Backend, opts ...Option) (*Repository, error) {
	var o options
	for _, f := range opts {
		f(&o)
	}
	if _, err := be.Size(ctx, "config"); err == nil {
		return nil, errors.New("repository already initialized at " + be.Location())
	} else if !errors.Is(err, backend.ErrNotFound) {
		return nil, err
	}
	pol, err := chunker.RandomPolynomial()
	if err != nil {
		return nil, err
	}
	var rid [16]byte
	if _, err := rand.Read(rid[:]); err != nil {
		return nil, err
	}
	polStr := fmt.Sprintf("%x", uint64(pol))
	cfg := Config{
		Version:           FormatVersion,
		ID:                hex.EncodeToString(rid[:]),
		Created:           time.Now().UTC(),
		ChunkerPolynomial: polStr,
		PackSize:          DefaultPackSize,
	}
	var key []byte
	if o.password != "" {
		mk, err := createKey(ctx, be, o.password, polStr)
		if err != nil {
			return nil, fmt.Errorf("create key: %w", err)
		}
		key = mk.Key
		cfg.Encryption = EncryptionAES256GCM
		cfg.ChunkerPolynomial = ""
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := be.Save(ctx, "config", b); err != nil {
		return nil, fmt.Errorf("save config: %w", err)
	}
	return newRepository(be, cfg, polStr, key)
}

// Open loads the config and index of an existing repository. Encrypted
// repositories need the Password option.
func Open(ctx context.Context, be backend.Backend, opts ...Option) (*Repository, error) {
	var o options
	for _, f := range opts {
		f(&o)
	}
	b, err := be.Load(ctx, "config")
	if errors.Is(err, backend.ErrNotFound) {
		return nil, fmt.Errorf("%s: %w", be.Location(), ErrNotInitialized)
	}
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if cfg.Version != FormatVersion {
		return nil, fmt.Errorf("unsupported repository version %d", cfg.Version)
	}
	polStr, key := cfg.ChunkerPolynomial, []byte(nil)
	switch cfg.Encryption {
	case "":
		// Someone who can write to the storage could replace config with an
		// unencrypted one (and remove index/ and snapshots/): the next backup
		// would then upload everything in plaintext. A caller that has a
		// recovery key expects an encrypted repository.
		if o.password != "" {
			return nil, fmt.Errorf("%s: %w", be.Location(), ErrNotEncrypted)
		}
	case EncryptionAES256GCM:
		mk, err := unlock(ctx, be, o.password)
		if err != nil {
			return nil, err
		}
		polStr, key = mk.ChunkerPolynomial, mk.Key
	default:
		return nil, fmt.Errorf("unsupported encryption %q", cfg.Encryption)
	}
	r, err := newRepository(be, cfg, polStr, key)
	if err != nil {
		return nil, err
	}
	if err := r.loadIndex(ctx, o.indexProgress); err != nil {
		return nil, err
	}
	return r, nil
}

func newRepository(be backend.Backend, cfg Config, polStr string, key []byte) (*Repository, error) {
	var p uint64
	if _, err := fmt.Sscanf(polStr, "%x", &p); err != nil {
		return nil, fmt.Errorf("config: bad chunker polynomial: %w", err)
	}
	pol := chunker.Pol(p)
	if !pol.Irreducible() {
		return nil, errors.New("config: chunker polynomial is not irreducible")
	}
	if cfg.PackSize <= 0 {
		cfg.PackSize = DefaultPackSize
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	// The decoder's memory is bounded: a damaged or tampered pack (in an
	// unencrypted repository nothing authenticates it) must not make a blob
	// expand into all the memory of the machine.
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0), zstd.WithDecoderMaxMemory(maxBlobRaw), zstd.WithDecoderMaxWindow(32<<20))
	if err != nil {
		return nil, err
	}
	var aead cipher.AEAD
	if key != nil {
		var err error
		if aead, err = newAEAD(key); err != nil {
			return nil, err
		}
	}
	r := &Repository{
		aead:       aead,
		be:         be,
		cfg:        cfg,
		pol:        pol,
		idx:        newIndex(),
		enc:        enc,
		dec:        dec,
		pack:       &packBuilder{},
		pending:    map[BlobHandle]struct{}{},
		maxUploads: backend.Concurrency(be),
	}
	r.upDone = sync.NewCond(&r.mu)
	return r, nil
}

func (r *Repository) Config() Config                 { return r.cfg }
func (r *Repository) Backend() backend.Backend       { return r.be }
func (r *Repository) Index() *Index                  { return r.idx }
func (r *Repository) ChunkerPolynomial() chunker.Pol { return r.pol }

// Concurrency is how many blobs may be read at once from the backend
// (restores read ahead on object stores; 1 elsewhere).
func (r *Repository) Concurrency() int { return max(r.maxUploads, 1) }

// Stats returns write statistics for this session.
func (r *Repository) Stats() WriteStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stats
}

// indexReaders is how many index files are read at once from an object
// store (they are small, so latency dominates: a large repository at a
// distant provider has thousands of them).
const indexReaders = 8

func (r *Repository) loadIndex(ctx context.Context, progress func(done, total int)) error {
	names, err := r.be.List(ctx, "index")
	if err != nil {
		return fmt.Errorf("list index: %w", err)
	}
	workers := 1
	if r.Concurrency() > 1 {
		workers = indexReaders
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan string)
	var (
		mu       sync.Mutex
		firstErr error
		done     int
		wg       sync.WaitGroup
	)
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		mu.Unlock()
	}
	for i := 0; i < min(workers, max(len(names), 1)); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range jobs {
				if err := r.loadIndexFile(ctx, n); err != nil {
					fail(err)
					continue
				}
				if progress != nil {
					mu.Lock()
					done++
					d := done
					mu.Unlock()
					progress(d, len(names))
				}
			}
		}()
	}
	for _, n := range names {
		if ctx.Err() != nil {
			break
		}
		jobs <- n
	}
	close(jobs)
	wg.Wait()
	return firstErr
}

func (r *Repository) loadIndexFile(ctx context.Context, n string) error {
	sealed, err := r.be.Load(ctx, n)
	if err != nil {
		return fmt.Errorf("load %s: %w", n, err)
	}
	b, err := r.open(sealed)
	if err != nil {
		return fmt.Errorf("%s: %w", n, err)
	}
	f, err := decodeIndexFile(b)
	if err != nil {
		return fmt.Errorf("parse %s: %w", n, err)
	}
	for _, p := range f.Packs {
		r.idx.addPack(p.ID, p.Blobs)
	}
	r.noteIndexFile(n)
	return nil
}

// SaveBlob stores data unless an identical blob already exists. It returns
// the blob ID and whether new data was written.
func (r *Repository) SaveBlob(ctx context.Context, t BlobType, data []byte) (ID, bool, error) {
	id := Hash(data)
	h := BlobHandle{Type: t, ID: id}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.pending[h]; ok || r.idx.Has(h) {
		r.stats.DupBlobs++
		return id, false, nil
	}

	var stored []byte
	if !r.noCompress {
		stored = r.enc.EncodeAll(data, nil)
	}
	rawLen := len(data)
	if r.noCompress || len(stored) >= len(data) {
		stored, rawLen = data, 0
	}
	stored = r.seal(stored)
	r.pack.add(t, id, stored, rawLen)
	r.pending[h] = struct{}{}
	r.stats.NewBlobs++
	r.stats.RawBytes += uint64(len(data))
	r.stats.StoredBytes += uint64(len(stored))

	if r.pack.size() >= r.cfg.PackSize {
		if err := r.flushPackLocked(ctx); err != nil {
			return id, true, err
		}
	}
	return id, true, nil
}

func (r *Repository) flushPackLocked(ctx context.Context) error {
	if r.upErr != nil {
		return r.upErr
	}
	if len(r.pack.blobs) == 0 {
		return nil
	}
	data, packID, blobs, err := r.pack.finish(r.seal)
	if err != nil {
		return err
	}
	r.pack = &packBuilder{}
	if r.maxUploads <= 1 {
		if err := r.be.Save(ctx, packName(packID), data); err != nil {
			// Sticky, as for parallel uploads: the pack's blobs stay pending,
			// so a later save of the same data would count as stored (a later
			// VM of the run saved without it). Nothing more can be saved.
			r.upErr = fmt.Errorf("upload pack %s: %w", packID.Short(), err)
			return r.upErr
		}
		return r.packUploadedLocked(ctx, packID, blobs)
	}
	// Wait for a free upload slot (Wait releases mu meanwhile, so other
	// writers keep filling the next pack).
	for r.inflight >= r.maxUploads && r.upErr == nil {
		r.upDone.Wait()
	}
	if r.upErr != nil {
		return r.upErr
	}
	r.inflight++
	go func() {
		err := r.be.Save(ctx, packName(packID), data)
		r.mu.Lock()
		defer r.mu.Unlock()
		defer r.upDone.Broadcast()
		r.inflight--
		if err == nil {
			err = r.packUploadedLocked(ctx, packID, blobs)
		} else {
			err = fmt.Errorf("upload pack %s: %w", packID.Short(), err)
		}
		if err != nil && r.upErr == nil {
			r.upErr = err
		}
	}()
	return nil
}

// packUploadedLocked indexes a pack that reached the backend.
func (r *Repository) packUploadedLocked(ctx context.Context, packID ID, blobs []PackedBlob) error {
	r.idx.addPack(packID, blobs)
	for _, b := range blobs {
		delete(r.pending, BlobHandle{Type: b.Type, ID: b.ID})
	}
	r.unindexed = append(r.unindexed, indexPack{ID: packID, Blobs: blobs})
	r.stats.PacksUploaded++
	if len(r.unindexed) >= indexFlushPacks {
		return r.writeIndexLocked(ctx)
	}
	return nil
}

// waitUploadsLocked waits until no pack upload is in flight and returns the
// first upload error.
func (r *Repository) waitUploadsLocked() error {
	for r.inflight > 0 {
		r.upDone.Wait()
	}
	return r.upErr
}

func (r *Repository) writeIndexLocked(ctx context.Context) error {
	if len(r.unindexed) == 0 {
		return nil
	}
	name, err := r.saveIndexFile(ctx, r.unindexed)
	if err != nil {
		return err
	}
	r.noteIndexFile(name)
	r.unindexed = nil
	return nil
}

// Flush uploads the current pack and writes an index for all new packs.
// It must be called before a snapshot referencing new blobs is saved.
func (r *Repository) Flush(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	err := r.flushPackLocked(ctx)
	// Packs still uploading must be in the index before it is written.
	if werr := r.waitUploadsLocked(); err == nil {
		err = werr
	}
	if err != nil {
		return err
	}
	return r.writeIndexLocked(ctx)
}

// LoadBlob reads, decompresses and verifies a blob.
func (r *Repository) LoadBlob(ctx context.Context, t BlobType, id ID) ([]byte, error) {
	loc, ok := r.idx.Lookup(BlobHandle{Type: t, ID: id})
	if !ok && r.maxUploads > 1 {
		// The blob may be in a pack that is still uploading.
		r.mu.Lock()
		_ = r.waitUploadsLocked()
		r.mu.Unlock()
		loc, ok = r.idx.Lookup(BlobHandle{Type: t, ID: id})
	}
	if !ok {
		return nil, fmt.Errorf("%s blob %s not found in index", t, id.Short())
	}
	stored, err := r.be.LoadRange(ctx, packName(loc.Pack), int64(loc.Offset), int(loc.Length))
	if err != nil {
		return nil, err
	}
	return r.decodeBlob(id, t, stored, loc.Raw)
}

func (r *Repository) decodeBlob(id ID, t BlobType, stored []byte, raw uint32) ([]byte, error) {
	data, err := r.open(stored)
	if err != nil {
		return nil, fmt.Errorf("%s blob %s: %w", t, id.Short(), err)
	}
	if raw > 0 {
		if raw > maxBlobRaw {
			return nil, fmt.Errorf("%s blob %s: invalid size %d", t, id.Short(), raw)
		}
		// A frame that announces another size than the index is not decoded:
		// the decoder would reserve memory for the announced size.
		var h zstd.Header
		if h.Decode(data) == nil && h.HasFCS && h.FrameContentSize != uint64(raw) {
			return nil, fmt.Errorf("%s blob %s: size does not match the index", t, id.Short())
		}
		// The size comes from the index; reserve at most 16 MiB up front.
		data, err = r.dec.DecodeAll(data, make([]byte, 0, min(raw, 16<<20)))
		if err != nil {
			return nil, fmt.Errorf("%s blob %s: decompress: %w", t, id.Short(), err)
		}
		if len(data) != int(raw) {
			return nil, fmt.Errorf("%s blob %s: size does not match the index", t, id.Short())
		}
	}
	if Hash(data) != id {
		return nil, fmt.Errorf("%s blob %s: hash mismatch (data corrupted)", t, id.Short())
	}
	return data, nil
}

// ReadPackHeader returns the blob list stored inside a pack.
func (r *Repository) ReadPackHeader(ctx context.Context, pack ID) ([]PackedBlob, error) {
	name := packName(pack)
	tr, err := r.be.LoadRange(ctx, name, -packTrailerLen, packTrailerLen)
	if err != nil {
		return nil, err
	}
	hl := binary.LittleEndian.Uint32(tr)
	if hl == 0 || hl > maxPackHeader {
		return nil, fmt.Errorf("pack %s: invalid header length %d", pack.Short(), hl)
	}
	hdr, err := r.be.LoadRange(ctx, name, -int64(packTrailerLen+hl), int(hl))
	if err != nil {
		return nil, err
	}
	if hdr, err = r.open(hdr); err != nil {
		return nil, fmt.Errorf("pack %s header: %w", pack.Short(), err)
	}
	return parsePackHeader(hdr)
}

// ListPacks returns the IDs of all pack files present in the backend.
func (r *Repository) ListPacks(ctx context.Context) ([]ID, error) {
	names, err := r.be.List(ctx, "data")
	if err != nil {
		return nil, err
	}
	var out []ID
	for _, n := range names {
		id, err := ParseID(n[strings.LastIndex(n, "/")+1:])
		if err != nil {
			continue
		}
		out = append(out, id)
	}
	return out, nil
}

// VerifyPack downloads a whole pack, checks its name hash, and verifies every
// blob listed in its header.
func (r *Repository) VerifyPack(ctx context.Context, pack ID) (int, error) {
	data, err := r.be.Load(ctx, packName(pack))
	if errors.Is(err, backend.ErrNotFound) {
		return 0, fmt.Errorf("pack %s: missing on the storage: %w", pack.Short(), ErrPackDamaged)
	}
	if err != nil {
		return 0, err // a read problem (network, timeout), no verdict on the pack
	}
	if Hash(data) != pack {
		// Fewer bytes arrived than the storage holds: the way lost them,
		// not the storage (#38: a host whose downloads were cut short).
		size, serr := r.be.Size(ctx, packName(pack))
		switch {
		case serr != nil:
			// Without the stored size nothing tells a cut download from a
			// damaged pack: no verdict (A8).
			return 0, fmt.Errorf("pack %s: content hash mismatch, and its size on the storage could not be read: %v", pack.Short(), serr)
		case size > int64(len(data)):
			return 0, fmt.Errorf("pack %s: only %d of its %d bytes arrived (the storage has it whole; check the network path: proxy, firewall, IPS, NAT)", pack.Short(), len(data), size)
		}
		return 0, fmt.Errorf("pack %s: content hash mismatch: %w", pack.Short(), ErrPackDamaged)
	}
	// From here the content is exactly what was written (its hash is its
	// name): what follows cannot be read differently on another try.
	if len(data) < packTrailerLen {
		return 0, fmt.Errorf("pack %s: too short: %w", pack.Short(), ErrPackDamaged)
	}
	hl := int(binary.LittleEndian.Uint32(data[len(data)-packTrailerLen:]))
	if hl <= 0 || hl > len(data)-packTrailerLen {
		return 0, fmt.Errorf("pack %s: invalid header length: %w", pack.Short(), ErrPackDamaged)
	}
	hdrStart := len(data) - packTrailerLen - hl
	hdr, err := r.open(data[hdrStart : len(data)-packTrailerLen])
	if err != nil {
		return 0, fmt.Errorf("pack %s header: %v: %w", pack.Short(), err, ErrPackDamaged)
	}
	blobs, err := parsePackHeader(hdr)
	if err != nil {
		return 0, fmt.Errorf("pack %s: %v: %w", pack.Short(), err, ErrPackDamaged)
	}
	for _, b := range blobs {
		end := int(b.Offset) + int(b.Length)
		if end > hdrStart {
			return 0, fmt.Errorf("pack %s: blob %s out of bounds: %w", pack.Short(), b.ID.Short(), ErrPackDamaged)
		}
		if _, err := r.decodeBlob(b.ID, b.Type, data[b.Offset:end], b.Raw); err != nil {
			return 0, fmt.Errorf("pack %s: %v: %w", pack.Short(), err, ErrPackDamaged)
		}
	}
	return len(blobs), nil
}

// SaveJSON stores v as JSON at dir/<sha256> and returns the ID.
func (r *Repository) saveJSONFile(ctx context.Context, dir string, v any) (ID, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ID{}, err
	}
	sealed := r.seal(b)
	id := Hash(sealed)
	return id, r.be.Save(ctx, dir+"/"+id.String(), sealed)
}

// SaveSnapshot stores a snapshot. Call Flush first.
func (r *Repository) SaveSnapshot(ctx context.Context, sn *Snapshot) (ID, error) {
	id, err := r.saveJSONFile(ctx, "snapshots", sn)
	if err == nil {
		sn.ID = id
	}
	return id, err
}

// LoadSnapshot loads a snapshot by full ID or unique prefix, or "latest".
func (r *Repository) LoadSnapshot(ctx context.Context, ref string) (*Snapshot, error) {
	if ref == "latest" {
		sns, err := r.ListSnapshots(ctx)
		if err != nil {
			return nil, err
		}
		if len(sns) == 0 {
			return nil, errors.New("repository has no snapshots")
		}
		return sns[len(sns)-1], nil
	}
	names, err := r.be.List(ctx, "snapshots")
	if err != nil {
		return nil, err
	}
	var match string
	for _, n := range names {
		base := n[strings.LastIndex(n, "/")+1:]
		if strings.HasPrefix(base, ref) {
			if match != "" {
				return nil, fmt.Errorf("snapshot prefix %q is ambiguous", ref)
			}
			match = n
		}
	}
	if match == "" {
		return nil, fmt.Errorf("snapshot %q not found", ref)
	}
	return r.loadSnapshotFile(ctx, match)
}

func (r *Repository) loadSnapshotFile(ctx context.Context, name string) (*Snapshot, error) {
	sealed, err := r.be.Load(ctx, name)
	if err != nil {
		return nil, err
	}
	id := Hash(sealed)
	if base := name[strings.LastIndex(name, "/")+1:]; base != id.String() {
		return nil, fmt.Errorf("snapshot %s: content hash mismatch", base)
	}
	b, err := r.open(sealed)
	if err != nil {
		return nil, fmt.Errorf("snapshot %s: %w", id.Short(), err)
	}
	var sn Snapshot
	if err := json.Unmarshal(b, &sn); err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	sn.ID = id
	return &sn, nil
}

// ListSnapshots returns all snapshots sorted by time (oldest first).
func (r *Repository) ListSnapshots(ctx context.Context) ([]*Snapshot, error) {
	names, err := r.be.List(ctx, "snapshots")
	if err != nil {
		return nil, err
	}
	var out []*Snapshot
	for _, n := range names {
		sn, err := r.loadSnapshotFile(ctx, n)
		if err != nil {
			return nil, err
		}
		out = append(out, sn)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, nil
}

// SaveTree stores a directory tree blob.
func (r *Repository) SaveTree(ctx context.Context, t *Tree) (ID, error) {
	b, err := json.Marshal(t)
	if err != nil {
		return ID{}, err
	}
	id, _, err := r.SaveBlob(ctx, TreeBlob, b)
	return id, err
}

// LoadTree loads a directory tree blob.
func (r *Repository) LoadTree(ctx context.Context, id ID) (*Tree, error) {
	b, err := r.LoadBlob(ctx, TreeBlob, id)
	if err != nil {
		return nil, err
	}
	var t Tree
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("tree %s: %w", id.Short(), err)
	}
	return &t, nil
}

// Close releases resources. It does not flush.
func (r *Repository) Close() error {
	// Uploads still in flight (a run that failed before Flush) must not use
	// a closed backend.
	r.mu.Lock()
	_ = r.waitUploadsLocked()
	r.mu.Unlock()
	_ = r.enc.Close()
	r.dec.Close()
	return r.be.Close()
}

// saveIndexFile writes an index file for packs and returns its name.
func (r *Repository) saveIndexFile(ctx context.Context, packs []indexPack) (string, error) {
	b, err := encodeIndexFile(packs)
	if err != nil {
		return "", err
	}
	sealed := r.seal(b)
	name := "index/" + Hash(sealed).String()
	if err := r.be.Save(ctx, name, sealed); err != nil {
		return "", fmt.Errorf("save index: %w", err)
	}
	return name, nil
}

// BlobLength returns the plaintext length of an indexed data blob.
func (r *Repository) BlobLength(id ID) (int, bool) {
	l, ok := r.idx.Lookup(BlobHandle{Type: DataBlob, ID: id})
	if !ok {
		return 0, false
	}
	if l.Raw > 0 {
		return int(l.Raw), true
	}
	n := int(l.Length)
	if r.aead != nil {
		n -= r.aead.NonceSize() + r.aead.Overhead()
	}
	return n, true
}

// Compression levels of new data (Job options): "" is the default.
const (
	CompressionDefault = ""
	CompressionOff     = "off"
	CompressionFast    = "fast"
	CompressionMax     = "max"
)

// SetCompression chooses how new blobs are compressed. Reading never
// depends on it: every blob records whether it is compressed.
func (r *Repository) SetCompression(level string) error {
	var lvl zstd.EncoderLevel
	switch level {
	case CompressionDefault:
		lvl = zstd.SpeedDefault
	case CompressionFast:
		lvl = zstd.SpeedFastest
	case CompressionMax:
		lvl = zstd.SpeedBestCompression
	case CompressionOff:
		r.mu.Lock()
		r.noCompress = true
		r.mu.Unlock()
		return nil
	default:
		return fmt.Errorf("unknown compression level %q", level)
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(lvl), zstd.WithEncoderConcurrency(1))
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.enc, r.encLevel, r.noCompress = enc, lvl, false
	r.mu.Unlock()
	return nil
}

func (r *Repository) noteIndexFile(name string) {
	r.indexMu.Lock()
	defer r.indexMu.Unlock()
	if r.indexFiles == nil {
		r.indexFiles = map[string]bool{}
	}
	r.indexFiles[name] = true
}

// refreshIndex brings the in-memory index up to date with the repository,
// after a lock is taken. The index is loaded when the repository is opened,
// which may be long before the lock (a run waits up to 30 minutes for
// another one's prune): without this, a backup could take blobs for stored
// that the prune deleted meanwhile, and a prune could delete packs another
// run wrote since. New index files are loaded; if one known is gone, a
// prune rewrote the index and all of it is loaded again.
func (r *Repository) refreshIndex(ctx context.Context) error {
	names, err := r.be.List(ctx, "index")
	if err != nil {
		return fmt.Errorf("list index: %w", err)
	}
	on := make(map[string]bool, len(names))
	for _, n := range names {
		on[n] = true
	}
	r.indexMu.Lock()
	gone := false
	var add []string
	for n := range r.indexFiles {
		if !on[n] {
			gone = true
			break
		}
	}
	if !gone {
		for _, n := range names {
			if !r.indexFiles[n] {
				add = append(add, n)
			}
		}
	}
	r.indexMu.Unlock()
	if gone {
		r.mu.Lock()
		r.idx = newIndex()
		r.mu.Unlock()
		r.indexMu.Lock()
		r.indexFiles = map[string]bool{}
		r.indexMu.Unlock()
		return r.loadIndex(ctx, nil)
	}
	for _, n := range add {
		if err := r.loadIndexFile(ctx, n); err != nil {
			return err
		}
	}
	return nil
}
