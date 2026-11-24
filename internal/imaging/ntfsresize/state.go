package ntfsresize

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// Errors Plan and MinimumSize refuse a source with, before anything is
// written. The messages they wrap say what is wrong and what to do.
var (
	// ErrInconsistent: the file records and $Bitmap disagree.
	ErrInconsistent = errors.New("the file system in the backup is inconsistent")
	// ErrNotClean: the volume is marked dirty, was not shut down cleanly or
	// holds an active hibernation image.
	ErrNotClean = errors.New("the file system in the backup was not shut down cleanly")
	// ErrUnsupported: a volume this package does not shrink.
	ErrUnsupported = errors.New("the file system cannot be shrunk")
)

// Hints appended to the refusals.
const (
	hintChkdsk   = `run "chkdsk /f" on the original volume, restart, and back it up again; or restore to a partition at least as large as the original`
	hintShutdown = `start that Windows, shut it down fully (shutdown /s /t 0, not with Fast Startup or hibernation) and back it up again; or restore to a partition at least as large as the original`
)

// $VOLUME_INFORMATION flags.
const (
	volumeDirty          = 0x0001
	volumeChkdskUnderway = 0x4000
)

// volumeInfo returns the NTFS version and the flags from $Volume.
func (v *Volume) volumeInfo() (major, minor byte, flags uint16, err error) {
	vi := v.records[recVolume].unnamed(attrVolumeInfo)
	if vi == nil {
		return 0, 0, 0, errors.New("$Volume has no volume information")
	}
	val := vi.residentValue()
	if len(val) < 12 {
		return 0, 0, 0, errors.New("$Volume has no volume information")
	}
	return val[8], val[9], binary.LittleEndian.Uint16(val[0x0A:]), nil
}

// checkState refuses volumes that cannot be shrunk safely, from what
// Open read: the NTFS version, the volume flags, bad clusters and (unless
// the image comes from a snapshot) the $LogFile restart area. The scan
// adds the consistency checks and the hibernation file.
func (v *Volume) checkState(opts PlanOptions) error {
	major, minor, flags, err := v.volumeInfo()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	if major != 3 || minor > 1 {
		return fmt.Errorf("%w: NTFS version %d.%d (only 3.0 and 3.1, Windows 2000 and later, are supported)", ErrUnsupported, major, minor)
	}
	if flags&volumeChkdskUnderway != 0 {
		return fmt.Errorf("%w: a chkdsk run was interrupted on this volume; %s", ErrNotClean, hintChkdsk)
	}
	if flags&volumeDirty != 0 {
		return fmt.Errorf("%w: the volume is marked for checking (dirty); %s", ErrNotClean, hintChkdsk)
	}
	bad := v.records[recBadClus].named(attrData, "$Bad")
	if bad == nil || !bad.NonResident {
		return fmt.Errorf("%w: $BadClus:$Bad not found", ErrUnsupported)
	}
	for _, r := range bad.Runs {
		if !r.Sparse {
			return fmt.Errorf("%w: the volume has bad clusters; run \"chkdsk /r\" on the original disk, or restore to a partition at least as large as the original", ErrUnsupported)
		}
	}
	if _, err := v.streamAttr(recLogFile, "$LogFile"); err != nil {
		return fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	if _, err := v.streamAttr(recMFTMirr, "$MFTMirr"); err != nil {
		return fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	if opts.FromSnapshot {
		// A shadow copy is taken of a mounted volume, so its log is open;
		// VSS flushed the metadata first, so nothing in it is lost.
		return nil
	}
	switch st, detail := v.logState(); st {
	case logClean, logEmpty:
		return nil
	case logOpen:
		return fmt.Errorf("%w: its $LogFile still holds open transactions (Windows was running, crashed or lost power when this copy was taken); %s", ErrNotClean, hintShutdown)
	case logCached:
		return fmt.Errorf("%w: Windows kept file system metadata in memory when this copy was taken (Fast Startup, hibernation, or the disk was cut off); %s", ErrNotClean, hintShutdown)
	default:
		return fmt.Errorf("%w: cannot tell whether it was shut down cleanly (%s); %s", ErrNotClean, detail, hintShutdown)
	}
}

// logStatus is what the $LogFile restart area says about the last
// shutdown.
type logStatus int

const (
	logClean      logStatus = iota // closed, or open with the clean flag
	logEmpty                       // all 0xFF: reset by chkdsk or a resize
	logOpen                        // in use without the clean flag
	logCached                      // version 2.0: Windows 8+ kept metadata cached
	logUnreadable                  // no valid restart page
)

// String names a log state for diagnostics.
func (s logStatus) String() string {
	return [...]string{"clean", "empty", "open (not shut down cleanly)", "metadata cached (Fast Startup or hibernation)", "unreadable"}[s]
}

// restartPage is the part of a $LogFile restart page that tells whether
// the volume was shut down cleanly.
type restartPage struct {
	sysPageSize  uint64
	major, minor uint16
	lsn          uint64
	inUse        uint16 // first in-use log client; 0xFFFF for none
	flags        uint16
}

// Restart area values.
const (
	logNoClient          = 0xFFFF
	restartVolumeIsClean = 0x0002
)

// logState reads the restart pages of $LogFile the way ntfs-3g does
// (ntfs_check_logfile, ntfs_is_logfile_clean): the newer valid page
// counts; version 2.0 means Windows kept metadata cached; an open log
// without the clean flag means an unclean shutdown.
func (v *Volume) logState() (logStatus, string) {
	lf, err := v.streamAttr(recLogFile, "$LogFile")
	if err != nil {
		return logUnreadable, err.Error()
	}
	if lf.DataSize < 4 {
		return logUnreadable, "$LogFile is empty"
	}
	head, err := v.readVirtual(lf.Runs, 0, 4)
	if err != nil {
		return logUnreadable, err.Error()
	}
	var pages []restartPage
	if p, ok := v.restartPage(lf, 0); ok {
		pages = append(pages, p)
		if q, ok := v.restartPage(lf, p.sysPageSize); ok {
			pages = append(pages, q)
		}
	} else {
		// The first page is torn: look for the second one where it can be.
		for pos := uint64(512); pos <= 64<<10; pos <<= 1 {
			if q, ok := v.restartPage(lf, pos); ok && q.sysPageSize == pos {
				pages = append(pages, q)
				break
			}
		}
	}
	if len(pages) == 0 {
		if bytes.Equal(head, []byte{0xFF, 0xFF, 0xFF, 0xFF}) {
			return logEmpty, ""
		}
		return logUnreadable, "no valid $LogFile restart page"
	}
	cur := pages[0]
	if len(pages) == 2 && pages[1].lsn > cur.lsn {
		cur = pages[1]
	}
	switch {
	case cur.major == 2 && cur.minor == 0:
		return logCached, ""
	case cur.major != 1:
		return logUnreadable, fmt.Sprintf("$LogFile version %d.%d", cur.major, cur.minor)
	case cur.inUse != logNoClient && cur.flags&restartVolumeIsClean == 0:
		return logOpen, ""
	}
	return logClean, ""
}

// restartPage reads and checks the restart page at byte pos of $LogFile.
func (v *Volume) restartPage(lf *Attribute, pos uint64) (restartPage, bool) {
	var rp restartPage
	if pos > lf.DataSize || lf.DataSize-pos < 512 {
		return rp, false
	}
	hdr, err := v.readVirtual(lf.Runs, pos, 512)
	if err != nil {
		return rp, false
	}
	magic := string(hdr[0:4])
	if magic != "RSTR" && magic != "CHKD" {
		return rp, false
	}
	sys := uint64(binary.LittleEndian.Uint32(hdr[16:]))
	logPage := uint64(binary.LittleEndian.Uint32(hdr[20:]))
	if sys < 512 || sys > 64<<10 || sys&(sys-1) != 0 || logPage < 512 || logPage&(logPage-1) != 0 || lf.DataSize-pos < sys {
		return rp, false
	}
	page, err := v.readVirtual(lf.Runs, pos, sys)
	if err != nil {
		return rp, false
	}
	// chkdsk may leave its page without an update sequence.
	if !(magic == "CHKD" && binary.LittleEndian.Uint16(page[6:]) == 0) {
		if removeFixups(page) != nil {
			return rp, false
		}
	}
	raOff := uint64(binary.LittleEndian.Uint16(page[24:]))
	if raOff&7 != 0 || raOff < 0x1E || raOff+16 > sys {
		return rp, false
	}
	ra := page[raOff:]
	rp = restartPage{
		sysPageSize: sys,
		minor:       binary.LittleEndian.Uint16(page[26:]),
		major:       binary.LittleEndian.Uint16(page[28:]),
		lsn:         binary.LittleEndian.Uint64(ra[0:]),
		inUse:       binary.LittleEndian.Uint16(ra[12:]),
		flags:       binary.LittleEndian.Uint16(ra[14:]),
	}
	return rp, true
}

// hiberSignatures start an active hibernation image (any case): "hibr"
// while hibernated, "rstr"/"wake" around a resume. Windows 8 and later
// clear the header after a successful resume.
var hiberSignatures = []string{"hibr", "wake", "rstr"}

// hiberActive tells whether file record num (hiberfil.sys) holds an
// active hibernation image, by the signature at the start of its data.
func (v *Volume) hiberActive(num uint64) (bool, string, error) {
	rec, err := v.readRecord(num)
	if err != nil {
		return false, "", err
	}
	a := firstDataSegment(rec)
	if a == nil {
		if al := rec.unnamed(attrAttributeList); al != nil {
			list, err := v.attrListBytes(al)
			if err != nil {
				return false, "", err
			}
			var ext uint64
			err = walkAttrList(list, func(t uint32, nameLen int, vcn, ref uint64) error {
				if t == attrData && nameLen == 0 && vcn == 0 {
					ext = ref & recordNumberMask
				}
				return nil
			})
			if err != nil {
				return false, "", err
			}
			if ext != 0 && ext != num {
				er, err := v.readRecord(ext)
				if err != nil {
					return false, "", err
				}
				if !er.Extension || er.Base != num {
					return false, "", fmt.Errorf("record %d does not belong to hiberfil.sys", ext)
				}
				a = firstDataSegment(er)
			}
		}
	}
	if a == nil {
		return false, "", nil
	}
	var head []byte
	if !a.NonResident {
		head = a.residentValue()
	} else if a.DataSize >= 4 {
		if head, err = v.readVirtual(a.Runs, 0, 4); err != nil {
			return false, "", err
		}
	}
	if len(head) < 4 {
		return false, "", nil
	}
	sig := strings.ToLower(string(head[:4]))
	for _, s := range hiberSignatures {
		if sig == s {
			return true, string(head[:4]), nil
		}
	}
	return false, "", nil
}

// firstDataSegment returns the unnamed $DATA of a record that starts at
// VCN 0.
func firstDataSegment(r *Record) *Attribute {
	for _, a := range dataSegments(r) {
		if !a.NonResident || a.StartVCN == 0 {
			return a
		}
	}
	return nil
}

// The volume header of the Volume Shadow Copy service sits at byte 0x1E00
// of the volume (inside $Boot): the VSS identifier, then the record type
// (1) at 20 and the byte offset of the shadow copy catalog at 48. The
// catalog and the stores refer to each other by volume byte offsets too.
const (
	vssHeaderOffset  = 0x1E00
	vssCatalogOffset = 48
)

// vssIdentifier is {3808876b-c176-4e48-b7ae-04046e6cc752} as stored.
var vssIdentifier = []byte{0x6b, 0x87, 0x08, 0x38, 0x76, 0xc1, 0x48, 0x4e, 0xb7, 0xae, 0x04, 0x04, 0x6e, 0x6c, 0xc7, 0x52}

// shadowCopyHeader reads the sector that holds the VSS volume header. It
// returns the sector with the catalog offset cleared, its byte offset,
// and whether the header points to a catalog (the volume has shadow
// copies). A volume without the header returns nil.
func (v *Volume) shadowCopyHeader() ([]byte, uint64, error) {
	at := uint64(vssHeaderOffset) / v.BytesPerSector * v.BytesPerSector
	sec := make([]byte, v.BytesPerSector)
	if err := v.readAt(sec, at); err != nil {
		return nil, 0, fmt.Errorf("read the shadow copy header: %w", err)
	}
	h := sec[vssHeaderOffset-at:]
	if !bytes.Equal(h[:16], vssIdentifier) || binary.LittleEndian.Uint32(h[20:]) != 1 || binary.LittleEndian.Uint64(h[vssCatalogOffset:]) == 0 {
		return nil, 0, nil
	}
	// Without a catalog volsnap finds no shadow copies and starts afresh;
	// the rest of the header (storage location, size limit) stays.
	binary.LittleEndian.PutUint64(h[vssCatalogOffset:], 0)
	return sec, at, nil
}

// StateReport describes what decides whether the volume may be shrunk,
// as far as Open read it: the NTFS version, the volume flags and the
// $LogFile state (a diagnostic; Check reports the rest).
func (v *Volume) StateReport() string {
	major, minor, flags, err := v.volumeInfo()
	if err != nil {
		return err.Error()
	}
	var f []string
	if flags&volumeDirty != 0 {
		f = append(f, "dirty")
	}
	if flags&volumeChkdskUnderway != 0 {
		f = append(f, "chkdsk underway")
	}
	if len(f) == 0 {
		f = append(f, "not dirty")
	}
	st, detail := v.logState()
	if detail != "" {
		detail = ": " + detail
	}
	return fmt.Sprintf("NTFS %d.%d, volume flags %#04x (%s), $LogFile %s%s", major, minor, flags, strings.Join(f, ", "), st, detail)
}
