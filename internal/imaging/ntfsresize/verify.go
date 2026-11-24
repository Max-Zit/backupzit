package ntfsresize

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// ErrVerify is wrapped by the errors of Verify: the target does not hold
// what the plan wrote.
var ErrVerify = errors.New("the shrunk file system does not check out")

// Verify reads the shrunk volume back from target (a reader over the
// target partition) after Apply and checks it before anything mounts it:
// the geometry and the boot sectors, $Bitmap, every file record the plan
// wrote and its mirror, the reset $LogFile, the dirty flag, the cleared
// shadow copy catalog, and the consistency of all file records against
// $Bitmap. It reads the target's whole $MFT once. The hidden sectors
// field of the boot sector is not compared, so it may run before or after
// the boot sector is adjusted for a moved partition.
func (p *Plan) Verify(target io.ReaderAt) error {
	v := p.v
	nv, err := Open(target)
	if err != nil {
		return fmt.Errorf("%w: reopen: %v", ErrVerify, err)
	}
	if nv.BytesPerSector != v.BytesPerSector || nv.ClusterSize != v.ClusterSize || nv.RecordSize != v.RecordSize ||
		nv.TotalSectors != p.NewTotalSectors || nv.Clusters != p.NewClusters {
		return fmt.Errorf("%w: geometry %d sectors / %d clusters, want %d / %d", ErrVerify, nv.TotalSectors, nv.Clusters, p.NewTotalSectors, p.NewClusters)
	}
	if p.Unchanged {
		return nil
	}
	var problems []string
	bad := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }

	bps := v.BytesPerSector
	for _, off := range []uint64{0, p.NewTotalSectors * bps} {
		sec := make([]byte, bps)
		if err := nv.readAt(sec, off); err != nil {
			bad("read boot sector at %d: %v", off, err)
		} else if !equalBoot(sec, p.boot) {
			bad("boot sector at byte %d differs from the one written", off)
		}
	}
	if p.sectorsOnly {
		return verifyResult(problems)
	}
	if !slices.Equal(nv.mftRuns, p.mftRuns) {
		bad("$MFT runs %v, want %v", nv.mftRuns, p.mftRuns)
	}
	if !bytes.Equal(nv.bitmap, p.bitmap) {
		bad("$Bitmap differs from the one written")
	}
	mirr := nv.records[recMFTMirr].unnamed(attrData)
	for _, rec := range p.dirty {
		want, err := rec.encode()
		if err != nil {
			return err
		}
		got, err := nv.readVirtual(nv.mftRuns, rec.Number*v.RecordSize, v.RecordSize)
		if err == nil {
			err = removeFixups(got)
		}
		if err != nil || !equalOutsideUSA(got, want) {
			bad("file record %d differs from the one written (%v)", rec.Number, err)
		}
		if rec.Number < p.mirrorRecords {
			m, err := nv.readVirtual(mirr.Runs, rec.Number*v.RecordSize, v.RecordSize)
			if err == nil {
				err = removeFixups(m)
			}
			if err != nil || !equalOutsideUSA(m, want) {
				bad("$MFTMirr copy of record %d differs from $MFT (%v)", rec.Number, err)
			}
		}
	}
	if st, _ := nv.logState(); st != logEmpty {
		bad("$LogFile was not reset (%s)", st)
	}
	if _, _, flags, err := nv.volumeInfo(); err != nil || flags&volumeDirty == 0 {
		bad("the volume is not marked for checking")
	}
	if p.vssSector != nil {
		sec := make([]byte, len(p.vssSector))
		if err := nv.readAt(sec, p.vssAt); err != nil || !bytes.Equal(sec, p.vssSector) {
			bad("the shadow copy header was not cleared")
		}
	}
	if len(problems) > 0 {
		return verifyResult(problems)
	}
	sr, err := nv.scan(nv.Clusters, false, planProblems, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrVerify, err)
	}
	if sr.nProblems > 0 {
		bad("%d inconsistencies: %s", sr.nProblems, strings.Join(sr.problems, "; "))
	}
	return verifyResult(problems)
}

// verifyResult joins the problems Verify found into one error.
func verifyResult(problems []string) error {
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrVerify, strings.Join(problems, "; "))
}

// equalBoot compares boot sectors except the hidden sectors field
// (0x1C-0x1F), which the restore adjusts for a moved partition.
func equalBoot(a, b []byte) bool {
	return len(a) == len(b) && bytes.Equal(a[:0x1C], b[:0x1C]) && bytes.Equal(a[0x20:], b[0x20:])
}
