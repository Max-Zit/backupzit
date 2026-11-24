package ntfsresize

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// scanProblemLimit stops a scan after this many problems: the volume is
// refused anyway, and crafted runs that all claim the whole volume would
// otherwise make the scan check every cluster again for every run.
const scanProblemLimit = 1000

// errStopScan ends a scan early.
var errStopScan = errors.New("stop the scan")

// scanResult is what one pass over every file record found.
type scanResult struct {
	// owned has a bit for every cluster a file record's run refers to.
	owned []byte
	// problems are the first inconsistencies found, nProblems all of them.
	problems  []string
	nProblems uint64
	// truncated: the scan stopped at scanProblemLimit problems.
	truncated bool
	// ownedEnd is one past the highest owned cluster; bootEnd the same
	// for $Boot, which never moves.
	ownedEnd, bootEnd uint64
	// hiber is the record of \hiberfil.sys, if hasHiber.
	hiber    uint64
	hasHiber bool
	// mftExt are the extension records of $MFT.
	mftExt []*Record
	// cands are the records with clusters at or after from (collected
	// only when planning moves).
	from  uint64
	cands []*Record
}

// scan reads every file record in use and builds the ownership map: each
// non-sparse run must lie inside the volume, its clusters must be
// allocated in $Bitmap and no other run may claim them. That is the
// check ntfs-3g's ntfsresize does before it moves anything
// (check_cluster_allocation, compare_bitmaps). With collect, records with
// clusters at or after from are kept for planning.
func (v *Volume) scan(from uint64, collect bool, maxProblems int, progress func(done, total uint64)) (*scanResult, error) {
	sr := &scanResult{owned: make([]byte, len(v.bitmap)), from: from}
	add := func(format string, a ...any) {
		sr.nProblems++
		if len(sr.problems) < maxProblems {
			sr.problems = append(sr.problems, fmt.Sprintf(format, a...))
		}
	}
	err := v.scanRecords(progress, func(rec *Record) error {
		if rec.Extension && rec.Base == recMFT {
			sr.mftExt = append(sr.mftExt, rec)
		}
		if rec.hasName(recRoot, "hiberfil.sys") {
			sr.hiber, sr.hasHiber = rec.Number, true
			if rec.Extension {
				sr.hiber = rec.Base
			}
		}
		cand := false
		for _, a := range rec.Attrs {
			if !a.NonResident || isBadClus(rec, a) {
				continue
			}
			for _, r := range a.Runs {
				if r.Sparse {
					continue
				}
				if !inVolume(r, v.Clusters) {
					add("record %d attribute %#x %q: clusters %d+%d lie past the end of the volume (%d clusters)", rec.Number, a.Type, a.Name, r.LCN, r.Len, v.Clusters)
					continue
				}
				if c, ok := firstClear(v.bitmap, r.LCN, r.Len); ok {
					add("record %d attribute %#x %q: cluster %d is in use but free in $Bitmap", rec.Number, a.Type, a.Name, c)
				}
				if c, ok := firstSet(sr.owned, r.LCN, r.Len); ok {
					add("cluster %d is used twice (record %d attribute %#x %q and another run)", c, rec.Number, a.Type, a.Name)
				}
				setBits(sr.owned, r.LCN, r.Len, true)
				end := r.LCN + r.Len
				sr.ownedEnd = max(sr.ownedEnd, end)
				if rec.Number == recBoot {
					sr.bootEnd = max(sr.bootEnd, end)
				}
				if end > from {
					cand = true
				}
			}
		}
		if collect && cand {
			sr.cands = append(sr.cands, rec)
		}
		if sr.nProblems >= scanProblemLimit {
			return errStopScan
		}
		return nil
	})
	if errors.Is(err, errStopScan) {
		sr.truncated, err = true, nil
	}
	if err != nil {
		return nil, err
	}
	return sr, nil
}

// checkScan turns the problems of a scan, and an active hibernation file,
// into the refusal Plan returns.
func (v *Volume) checkScan(sr *scanResult) error {
	if sr.nProblems > 0 {
		more := sr.more()
		return fmt.Errorf("%w (%s%s); %s", ErrInconsistent, strings.Join(sr.problems, "; "), more, hintChkdsk)
	}
	if sr.hasHiber {
		active, sig, err := v.hiberActive(sr.hiber)
		if err != nil {
			return fmt.Errorf("%w: hiberfil.sys cannot be read: %v", ErrInconsistent, err)
		}
		if active {
			return fmt.Errorf("%w: hiberfil.sys holds an active hibernation image (%q): Windows was hibernated or shut down with Fast Startup when this copy was taken; %s", ErrNotClean, sig, hintShutdown)
		}
	}
	return nil
}

// Check reads every file record and reports inconsistencies: runs that
// reach past the end of the volume, clusters in use by a file but free in
// $Bitmap, and clusters used by two runs, then an active hibernation
// file. A healthy volume returns none. limit caps the number of problems
// reported. StateReport tells the volume flags and the $LogFile state.
func (v *Volume) Check(limit int) ([]string, error) {
	sr, err := v.scan(v.Clusters, false, limit, nil)
	if err != nil {
		return nil, err
	}
	out := sr.problems
	if more := sr.more(); more != "" {
		out = append(out, more[1:])
	}
	if sr.hasHiber {
		if active, sig, err := v.hiberActive(sr.hiber); err != nil {
			out = append(out, fmt.Sprintf("hiberfil.sys cannot be read: %v", err))
		} else if active {
			out = append(out, fmt.Sprintf("hiberfil.sys holds an active hibernation image (%q)", sig))
		}
	}
	return out, nil
}

// DumpRecord describes the non-resident attributes of one file record (a
// diagnostic): header fields, mapping pairs as stored and decoded.
func (v *Volume) DumpRecord(num uint64) (string, error) {
	r, err := v.readRecord(num)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "record %d in use %v extension %v base %d, %d attributes\n", num, r.InUse, r.Extension, r.Base, len(r.Attrs))
	for _, a := range r.Attrs {
		if !a.NonResident {
			fmt.Fprintf(&sb, "  attr %#x %q resident, %d bytes\n", a.Type, a.Name, len(a.raw))
			continue
		}
		flags := binary.LittleEndian.Uint16(a.raw[0x0C:])
		mp := int(binary.LittleEndian.Uint16(a.raw[0x20:]))
		cu := binary.LittleEndian.Uint16(a.raw[0x22:])
		fmt.Fprintf(&sb, "  attr %#x %q flags %#x len %d mp@%#x cu %d vcn %d-%d alloc %d data %d init %d\n",
			a.Type, a.Name, flags, len(a.raw), mp, cu, a.StartVCN, a.LastVCN, a.AllocSize, a.DataSize, a.InitializedSize)
		fmt.Fprintf(&sb, "    pairs % x\n", a.raw[mp:])
		total, _ := runClusters(a.Runs)
		fmt.Fprintf(&sb, "    %d runs, %d clusters: %v\n", len(a.Runs), total, a.Runs)
	}
	return sb.String(), nil
}

// BitmapReport compares $Bitmap with the clusters files actually use (a
// diagnostic): bits set without an owner, owned clusters missing, the
// bits past the last cluster, and the size of $Bitmap.
func (v *Volume) BitmapReport() (string, error) {
	sr, err := v.scan(v.Clusters, false, 0, nil)
	if err != nil {
		return "", err
	}
	var unowned, missing []uint64
	var nUnowned, nMissing uint64
	for c := uint64(0); c < v.Clusters; c++ {
		u, o := v.used(c), sr.owned[c/8]&(1<<(c%8)) != 0
		switch {
		case u && !o:
			nUnowned++
			if len(unowned) < 10 {
				unowned = append(unowned, c)
			}
		case o && !u:
			nMissing++
			if len(missing) < 10 {
				missing = append(missing, c)
			}
		}
	}
	var tail []int
	for c := v.Clusters; c < uint64(len(v.bitmap))*8 && len(tail) < 72; c++ {
		if v.used(c) {
			tail = append(tail, 1)
		} else {
			tail = append(tail, 0)
		}
	}
	bm := v.records[recBitmap].unnamed(attrData)
	return fmt.Sprintf("bitmap: %d bytes read, data size %d, alloc %d, init %d, need %d bytes for %d clusters\n"+
		"used but no owner: %d %v\nowned but free: %d %v\nbits past the end (first 72): %v\n",
		len(v.bitmap), bm.DataSize, bm.AllocSize, bm.InitializedSize, (v.Clusters+7)/8, v.Clusters,
		nUnowned, unowned, nMissing, missing, tail), nil
}

// more tells how many problems the list leaves out (" and N more", or
// " and more" when the scan stopped early), or "".
func (sr *scanResult) more() string {
	switch n := uint64(len(sr.problems)); {
	case sr.truncated:
		return " and more"
	case sr.nProblems > n:
		return fmt.Sprintf(" and %d more", sr.nProblems-n)
	}
	return ""
}
