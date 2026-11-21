package repo

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/max-zit/backupzit/internal/backend"
)

// errIndexDamaged marks an index file that was read but cannot be opened or
// parsed (cut short, overwritten, wrong key). Reading it again does not help.
var errIndexDamaged = errors.New("index file damaged")

// DamagedIndexFiles returns the index files that could not be read when the
// repository was opened or its index refreshed. Their packs were found from
// the pack headers instead (see recoverIndex); RebuildIndex replaces them.
func (r *Repository) DamagedIndexFiles() []string {
	r.indexMu.Lock()
	defer r.indexMu.Unlock()
	out := append([]string(nil), r.damagedIndex...)
	sort.Strings(out)
	return out
}

func (r *Repository) setDamagedIndex(names []string) {
	r.indexMu.Lock()
	defer r.indexMu.Unlock()
	r.damagedIndex = append([]string(nil), names...)
	if len(names) == 0 {
		r.unreadablePacks = nil
	}
}

// errDamagedIndexFiles is returned by operations that delete data on the
// strength of the index while the index is not trustworthy.
func (r *Repository) errDamagedIndexFiles() error {
	if bad := r.DamagedIndexFiles(); len(bad) > 0 {
		return fmt.Errorf("the index has damaged files (%s); run repair first", strings.Join(bad, ", "))
	}
	return nil
}

// recoverIndex adds the packs that no readable index file lists, from their
// own headers: the index is only an accelerator, every pack carries the
// list of its blobs. Without this, one damaged index file made the whole
// repository unusable, check and repair included, although all data was
// there (B11).
//
// The packs are marked recovered: their blobs can be read, but a new backup
// does not deduplicate against them (Index.Reusable), because a pack that
// repair dropped as damaged has an intact header and would otherwise come
// back. A storage error while reading a header fails the call instead of
// leaving the pack out: prune deletes every pack the index lacks. Only a
// pack whose header content is bad is skipped and remembered as unreadable;
// RebuildIndex decides about it.
func (r *Repository) recoverIndex(ctx context.Context) error {
	packs, err := r.ListPacks(ctx)
	if err != nil {
		return fmt.Errorf("list packs to recover the damaged index: %w", err)
	}
	indexed := r.idx.Packs()
	unreadable := map[ID]string{}
	for _, p := range packs {
		if _, ok := indexed[p]; ok {
			continue
		}
		blobs, err := r.ReadPackHeader(ctx, p)
		switch {
		case err == nil:
			r.idx.addRecoveredPack(p, blobs)
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, errPackHeader):
			unreadable[p] = err.Error()
		default:
			// A pack too short to hold a trailer is damaged, not a storage
			// problem; a pack that vanished since the listing is not there.
			size, serr := r.be.Size(ctx, packName(p))
			switch {
			case errors.Is(serr, backend.ErrNotFound):
			case serr == nil && size < packTrailerLen:
				unreadable[p] = fmt.Sprintf("pack %s: only %d bytes", p.Short(), size)
			default:
				return fmt.Errorf("read the header of pack %s to recover the damaged index: %w", p.Short(), err)
			}
		}
	}
	r.indexMu.Lock()
	r.unreadablePacks = unreadable
	r.indexMu.Unlock()
	return nil
}

// RebuildResult is what RebuildIndex did.
type RebuildResult struct {
	// Packs is the number of packs the new index lists.
	Packs int
	// Left are packs that are on the storage but not in the new index,
	// because they are proven damaged (a pack that fails its checks). Prune
	// removes them later.
	Left []ID
}

// RebuildIndex writes the index anew from what is in memory (the readable
// index files plus the packs recovered from their headers) and removes the
// old index files, the damaged ones included. The caller holds the
// exclusive lock.
//
// A recovered pack enters the new index only after VerifyPack has read it
// whole: the new index makes its blobs reusable by new backups, and a pack
// that repair dropped (or any damaged one) must not return. A pack that
// VerifyPack proves damaged is left out of the index (Left). If a pack
// cannot be verified for any other reason (storage error, timeout, header
// bad but no verdict), the call fails before it removes anything: the old
// index files, the damaged ones included, may be the only record of that
// pack's blobs. Packs with an unreadable header are verified the same way.
func (r *Repository) RebuildIndex(ctx context.Context) (*RebuildResult, error) {
	if err := r.Flush(ctx); err != nil {
		return nil, err
	}
	res := &RebuildResult{}
	skip := map[ID]bool{}
	extra := map[ID][]PackedBlob{}
	check := r.idx.RecoveredPacks()
	r.indexMu.Lock()
	for p := range r.unreadablePacks {
		check = append(check, p)
	}
	r.indexMu.Unlock()
	recovered := map[ID]bool{}
	for _, p := range r.idx.RecoveredPacks() {
		recovered[p] = true
	}
	for _, p := range check {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		_, err := r.VerifyPack(ctx, p)
		switch {
		case err == nil:
			if !recovered[p] { // header unreadable before, readable now
				blobs, herr := r.ReadPackHeader(ctx, p)
				if herr != nil {
					return nil, fmt.Errorf("pack %s: %w; the index files were not touched", p.Short(), herr)
				}
				extra[p] = blobs
			}
		case errors.Is(err, ErrPackDamaged):
			skip[p] = true
			res.Left = append(res.Left, p)
		default:
			return nil, fmt.Errorf("pack %s could not be verified, so the index files were not touched (try again): %w", p.Short(), err)
		}
	}
	keep := map[ID][]PackedBlob{}
	r.idx.Each(func(h BlobHandle, l Location) {
		if skip[l.Pack] {
			return
		}
		keep[l.Pack] = append(keep[l.Pack], PackedBlob{Type: h.Type, ID: h.ID, Offset: l.Offset, Length: l.Length, Raw: l.Raw})
	})
	for p, blobs := range extra {
		keep[p] = blobs
	}
	if err := r.rewriteIndex(ctx, keep); err != nil {
		return nil, err
	}
	res.Packs = len(keep)
	return res, nil
}

// rewriteIndex replaces all index files by new ones listing keep, then
// loads the index again.
func (r *Repository) rewriteIndex(ctx context.Context, keep map[ID][]PackedBlob) error {
	oldIndexes, err := r.be.List(ctx, "index")
	if err != nil {
		return fmt.Errorf("list index: %w", err)
	}
	list := make([]indexPack, 0, len(keep))
	for p, blobs := range keep {
		list = append(list, indexPack{ID: p, Blobs: blobs})
	}
	written := map[string]bool{}
	for start := 0; start < len(list); start += indexFlushPacks * 8 {
		end := min(start+indexFlushPacks*8, len(list))
		name, err := r.saveIndexFile(ctx, list[start:end])
		if err != nil {
			return err
		}
		written[name] = true
	}
	for _, n := range oldIndexes {
		if written[n] {
			continue
		}
		if err := r.be.Remove(ctx, n); err != nil && !strings.Contains(err.Error(), "not found") {
			return fmt.Errorf("remove %s: %w", n, err)
		}
	}
	r.mu.Lock()
	r.idx = newIndex()
	r.mu.Unlock()
	r.indexMu.Lock()
	r.indexFiles = map[string]bool{}
	r.indexMu.Unlock()
	return r.loadIndex(ctx, nil)
}
