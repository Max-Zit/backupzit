package repo

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/max-zit/backupzit/internal/backend"
)

// errIndexDamaged marks an index file that was read but cannot be opened or
// parsed (cut short, overwritten, wrong key). Reading it again does not help.
var errIndexDamaged = errors.New("index file damaged")

// ErrPackGone marks a pack that is not on the storage (any more), with
// ErrPackDamaged: damaged for the data check, but a rebuild that just listed
// it stops on it, and a check that finds most packs gone judges nothing.
var ErrPackGone = errors.New("missing on the storage")

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

// stillDamaged is the damaged list after a load of the index files listed:
// the files that failed now, plus the earlier ones that are not listed any
// more. A damaged file that disappears does not clear the list, because the
// packs only it listed are recovered from their headers until RebuildIndex
// has written the new index (which is what clears it).
func (r *Repository) stillDamaged(failed, listed []string) []string {
	out := append([]string(nil), failed...)
	r.indexMu.Lock()
	defer r.indexMu.Unlock()
	for _, n := range r.damagedIndex {
		if !slices.Contains(listed, n) && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
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
	// A listing can come back empty without an error (a data folder that is
	// not mounted, a share that lists nothing): the checker refuses to judge
	// then (H-N6), and so does the recovery. Packs missing from the listing
	// would be neither recovered nor remembered, and the new index would
	// leave them out.
	if len(packs) == 0 && len(indexed) > 0 {
		return fmt.Errorf("the storage lists no data packs at all although the index names %d: a folder may be missing or not mounted; nothing was recovered", len(indexed))
	}
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
	// because they are proven damaged (every try to verify them says so).
	// Prune removes them later.
	Left []ID
	// Blobs is the number of blobs that only the left packs held.
	Blobs int
	// Snapshots need data of the left packs (they cannot be restored
	// completely); Unchecked could not be read completely for other reasons
	// (kept, not counted as affected). The same as for DropDamagedPacks.
	Snapshots []*Snapshot
	Unchecked []*Snapshot
	// Moved are the damaged index files, now under index-damaged/.
	Moved []string
	// Missing is the number of blobs that backups use and no pack holds;
	// only with RebuildOptions.AcceptLost (their backups are in Snapshots).
	Missing int
}

// RebuildOptions control RebuildIndex.
type RebuildOptions struct {
	// AcceptLost lets the rebuild go on when backups use blobs that are in
	// no pack at all (the data is lost, or a pack the storage keeps hiding):
	// those backups are then reported as affected (Snapshots), for the
	// caller to remove them if the user asked so ("remove affected
	// backups"). Only when two more listings of the packs agree: a storage
	// that lists differently each time is no proof of anything.
	AcceptLost bool
}

// Repair is the result in the form of DropDamagedPacks, for the callers that
// report both the same way.
func (rb *RebuildResult) Repair() *RepairResult {
	return &RepairResult{Packs: len(rb.Left), Blobs: rb.Blobs, Snapshots: rb.Snapshots, Unchecked: rb.Unchecked}
}

// damagedIndexDir is where RebuildIndex moves damaged index files: not under
// index/, so that no listing of the index (open, check, prune) reads them.
const damagedIndexDir = "index-damaged"

// RebuildIndex writes the index anew from what is in memory (the readable
// index files plus the packs recovered from their headers), removes the old
// readable index files and moves the damaged ones to index-damaged/. The
// caller holds the exclusive lock.
//
// The damaged index file may be the only record of some packs, so nothing
// is touched until these hold:
//   - The packs are listed again; an empty listing although the index names
//     packs is refused (the checker's H-N6 guard).
//   - A recovered pack enters the new index only after it was verified: the
//     new index makes its blobs reusable by new backups, and a pack that
//     repair dropped (or any damaged one) must not return. It is left out
//     (Left) only when every one of three tries says it is damaged (as the
//     data check does: over plain HTTP one corrupt download can be a
//     one-off). Any other outcome (storage error, timeout, tries that
//     disagree) fails the call.
//   - Every blob that a backup uses is in the new index or in a pack proven
//     damaged. A pack that no listing showed would otherwise be missing from
//     the new index, and the next prune would delete it.
//
// The entries of a verified recovered pack come from its own header, so a
// blob whose chosen copy is in a damaged pack is still listed through the
// other pack that holds it.
func (r *Repository) RebuildIndex(ctx context.Context) (*RebuildResult, error) {
	return r.RebuildIndexWith(ctx, RebuildOptions{})
}

// RebuildIndexWith is RebuildIndex with options.
func (r *Repository) RebuildIndexWith(ctx context.Context, o RebuildOptions) (*RebuildResult, error) {
	if err := r.Flush(ctx); err != nil {
		return nil, err
	}
	untouched := func(err error) error { return fmt.Errorf("%w; the index files were not touched", err) }
	// List again under the lock: packs a former listing missed, and the
	// guard against an empty one.
	if err := r.recoverIndex(ctx); err != nil {
		return nil, untouched(err)
	}
	recovered := map[ID]bool{}
	var check []ID
	for _, p := range r.idx.RecoveredPacks() {
		recovered[p] = true
		check = append(check, p)
	}
	r.indexMu.Lock()
	for p := range r.unreadablePacks {
		check = append(check, p)
	}
	r.indexMu.Unlock()
	sort.Slice(check, func(i, j int) bool { return check[i].String() < check[j].String() })

	res := &RebuildResult{}
	skip := map[ID]bool{}
	verified := map[ID][]PackedBlob{}
	for _, p := range check {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var blobs []PackedBlob
		everyTryDamaged := true
		err := Tries(ctx, 0, func() (err error) {
			blobs, err = r.verifyPack(ctx, p)
			if !errors.Is(err, ErrPackDamaged) {
				everyTryDamaged = false
			}
			return err
		})
		switch {
		case errors.Is(err, ErrPackGone):
			// It was listed moments ago, under the exclusive lock: a pack
			// that vanishes now is a storage that changes or a folder that
			// went away, not damage (0.59 review: a data volume unmounted
			// during the rebuild left every later pack out).
			return nil, untouched(fmt.Errorf("pack %s was listed but is gone now: the storage changed during the repair or a folder went away", p.Short()))
		case err == nil:
			verified[p] = blobs
		case errors.Is(err, ErrPackTooLarge):
			// Neither verified nor proven damaged (MED 5, 0.63): trying again
			// does not help, the file has to be looked at.
			return nil, untouched(fmt.Errorf("%w; no BackupZit pack is that large: check this file on the storage and move it out of the data folder if it is not one, then repair again", err))
		case errors.Is(err, ErrPackDamaged) && everyTryDamaged:
			skip[p] = true
			res.Left = append(res.Left, p)
		default:
			return nil, fmt.Errorf("pack %s could not be verified, so the index files were not touched (try again): %w", p.Short(), err)
		}
	}

	// The new index: indexed packs as they are in memory, recovered ones
	// from their own headers.
	keep := map[ID][]PackedBlob{}
	inPacksLeft := map[BlobHandle]bool{}
	r.idx.Each(func(h BlobHandle, l Location) {
		_, fromHeader := verified[l.Pack]
		switch {
		case skip[l.Pack]:
			inPacksLeft[h] = true
		case recovered[l.Pack] || fromHeader:
			// listed from the header below
		default:
			keep[l.Pack] = append(keep[l.Pack], PackedBlob{Type: h.Type, ID: h.ID, Offset: l.Offset, Length: l.Length, Raw: l.Raw})
		}
	})
	for p, blobs := range verified {
		keep[p] = blobs
	}
	inNew := map[BlobHandle]bool{}
	for _, blobs := range keep {
		for _, b := range blobs {
			inNew[BlobHandle{Type: b.Type, ID: b.ID}] = true
		}
	}
	lost := map[BlobHandle]bool{}
	for h := range inPacksLeft {
		if !inNew[h] {
			lost[h] = true
		}
	}
	res.Blobs = len(lost)

	if err := r.checkUsedBlobs(ctx, res, lost, inNew, o.AcceptLost); err != nil {
		return nil, untouched(err)
	}
	moved, err := r.rewriteIndex(ctx, keep)
	if err != nil {
		return nil, err
	}
	res.Moved = moved
	res.Packs = len(keep)
	return res, nil
}

// checkUsedBlobs refuses the rebuild when a blob that a backup uses is
// neither in the new index (inNew) nor in a pack proven damaged (lost). The
// backups that need blobs of the left packs are listed in res, as
// DropDamagedPacks does. Their other blobs are checked too, with the lost
// folder lists and block maps as dead ends: a backup that needs a damaged
// pack may also need an intact one the listing hid, which the next prune
// would delete (0.59 review).
//
// The backups that use blobs no pack has are named in the error. With
// acceptLost (the user asked to remove affected backups) they are reported
// as affected instead, when two more listings of the packs agree.
func (r *Repository) checkUsedBlobs(ctx context.Context, res *RebuildResult, lost, inNew map[BlobHandle]bool, acceptLost bool) error {
	sns, err := r.ListSnapshots(ctx)
	if err != nil {
		return fmt.Errorf("read the backups to check that the new index lists everything they use: %w", err)
	}
	affected := map[ID]bool{}
	if len(lost) > 0 {
		trees := map[ID]bool{}
		for _, sn := range sns {
			hit, err := r.needsLost(ctx, sn, lost, trees)
			switch {
			case err != nil:
				res.Unchecked = append(res.Unchecked, sn)
			case hit:
				res.Snapshots = append(res.Snapshots, sn)
				affected[sn.ID] = true
			}
		}
	}
	// The blobs every backup uses, as prune collects them, not reading
	// what is lost.
	used, err := r.blobsOf(ctx, sns, lost)
	if err != nil {
		return fmt.Errorf("read the backups to check that the new index lists everything they use: %w", err)
	}
	missing := map[BlobHandle]bool{}
	for h := range used {
		if !inNew[h] && !lost[h] {
			missing[h] = true
		}
	}
	if len(missing) == 0 {
		return nil
	}
	// Which backups use them.
	var users []*Snapshot
	trees := map[ID]bool{}
	for _, sn := range sns {
		if hit, err := r.needsLost(ctx, sn, missing, trees); err == nil && hit {
			users = append(users, sn)
		}
	}
	if acceptLost {
		stable, err := r.packListingStable(ctx)
		if err != nil {
			return err
		}
		if stable {
			for _, sn := range users {
				if !affected[sn.ID] {
					res.Snapshots = append(res.Snapshots, sn)
					affected[sn.ID] = true
				}
			}
			res.Missing = len(missing)
			return nil
		}
	}
	var names []string
	for i, sn := range users {
		if i == 5 {
			names = append(names, fmt.Sprintf("and %d more", len(users)-5))
			break
		}
		names = append(names, fmt.Sprintf("%s of %s", sn.ID.Short(), sn.Time.Local().Format("2006-01-02 15:04")))
	}
	what := "backups"
	if len(names) > 0 {
		what = "backups " + strings.Join(names, ", ")
	}
	hint := "check that every data folder of the storage is there (mounted, complete) and repair again; if the data is really lost, repair with \"remove affected backups\" to remove these backups, which cannot be restored completely"
	if acceptLost {
		hint = "the storage lists its packs differently each time, so the backups were not removed; check the storage and repair again"
	}
	return fmt.Errorf("%d blobs that %s use are in no pack of the new index and in no pack proven damaged: %s", len(missing), what, hint)
}

// packListingStable lists the packs twice more and reports whether both
// listings name the same packs, all known to the index in memory.
func (r *Repository) packListingStable(ctx context.Context) (bool, error) {
	var sets [2]map[ID]bool
	for i := range sets {
		packs, err := r.ListPacks(ctx)
		if err != nil {
			return false, fmt.Errorf("list packs: %w", err)
		}
		sets[i] = map[ID]bool{}
		for _, p := range packs {
			sets[i][p] = true
		}
	}
	if len(sets[0]) != len(sets[1]) || len(sets[0]) == 0 {
		return false, nil
	}
	for p := range sets[0] {
		if !sets[1][p] {
			return false, nil
		}
	}
	return true, nil
}

// DamagedIndexCopy returns what the damaged index file name (index/...)
// held, from its copy under index-damaged/, which is sealed with the
// repository's key: for a look at it when support asks.
func (r *Repository) DamagedIndexCopy(ctx context.Context, name string) ([]byte, error) {
	b, err := r.be.Load(ctx, damagedIndexDir+"/"+strings.TrimPrefix(name, "index/"))
	if err != nil {
		return nil, err
	}
	return r.open(b)
}

// copyAside copies a damaged index file to index-damaged/. The caller
// removes it from index/ only after this succeeded: the copy holds every
// byte the storage has, so a file cut short on its way here is not taken for
// the whole file. It reports false for a file that is gone already.
func (r *Repository) copyAside(ctx context.Context, name string) (bool, error) {
	data, err := r.be.Load(ctx, name)
	if errors.Is(err, backend.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load %s to move it aside: %w", name, err)
	}
	size, err := r.be.Size(ctx, name)
	if err != nil {
		return false, fmt.Errorf("size of %s: %w", name, err)
	}
	if size != int64(len(data)) {
		return false, fmt.Errorf("%s: only %d of its %d bytes arrived; it was not moved aside", name, len(data), size)
	}
	// The copy keeps the folders below index/ (names stay apart) and is
	// sealed with the repository's key: whatever the storage handed over as
	// this file (a planted link to a file of the host, say) is not written
	// back readable (0.59 review).
	to := damagedIndexDir + "/" + strings.TrimPrefix(name, "index/")
	if err := r.be.Save(ctx, to, r.seal(data)); err != nil {
		return false, fmt.Errorf("copy %s to %s: %w", name, to, err)
	}
	return true, nil
}

// rewriteIndex replaces all index files by new ones listing keep, then
// loads the index again. The damaged index files are copied to
// index-damaged/ first (their names are returned) and removed afterwards;
// the others are removed. The damaged state ends here, and only here.
func (r *Repository) rewriteIndex(ctx context.Context, keep map[ID][]PackedBlob) ([]string, error) {
	oldIndexes, err := r.be.List(ctx, "index")
	if err != nil {
		return nil, fmt.Errorf("list index: %w", err)
	}
	// Before anything is written: a new file can have the name of a damaged
	// one (the same packs, cut short in place) and then replaces it.
	var moved []string
	for _, n := range r.DamagedIndexFiles() {
		ok, err := r.copyAside(ctx, n)
		if err != nil {
			return nil, err
		}
		if ok {
			moved = append(moved, n)
		}
	}
	list := make([]indexPack, 0, len(keep))
	for p, blobs := range keep {
		list = append(list, indexPack{ID: p, Blobs: blobs})
	}
	written := map[string]bool{}
	names, err := r.saveIndexFiles(ctx, list)
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		written[n] = true
	}
	for _, n := range oldIndexes {
		if written[n] {
			continue
		}
		if err := r.be.Remove(ctx, n); err != nil && !strings.Contains(err.Error(), "not found") {
			return nil, fmt.Errorf("remove %s: %w", n, err)
		}
	}
	r.setDamagedIndex(nil)
	r.mu.Lock()
	r.idx = newIndex()
	r.mu.Unlock()
	r.indexMu.Lock()
	r.indexFiles = map[string]bool{}
	r.indexMu.Unlock()
	return moved, r.loadIndex(ctx, nil)
}
