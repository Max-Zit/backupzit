//go:build windows

package fsutil

import (
	"errors"
	"os"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// fileID identifies a file: volume serial number and file index.
type fileID struct {
	vol uint32
	idx uint64
}

// lstat looks at the entry name of d without following it: the entry is
// opened relative to the folder's handle (no path is resolved again) as
// the link or folder itself, and a link's owner and target are read from
// that handle.
func (d *Dir) lstat(name string) (entry, error) {
	f, err := d.handle()
	if err != nil {
		return entry{}, err
	}
	h, err := openRelative(windows.Handle(f.Fd()), name)
	if err != nil {
		if errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND) || errors.Is(err, windows.STATUS_OBJECT_PATH_NOT_FOUND) {
			return entry{kind: entryMissing}, nil
		}
		return entry{}, &os.PathError{Op: "open", Path: d.Join(name), Err: err}
	}
	defer windows.CloseHandle(h) //nolint:errcheck // read-only handle
	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
		return entry{}, err
	}
	e := entry{id: fileID{fi.VolumeSerialNumber, uint64(fi.FileIndexHigh)<<32 | uint64(fi.FileIndexLow)}}
	dir := fi.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	if fi.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		// FILE_ATTRIBUTE_TAG_INFO
		var ti struct{ attrs, tag uint32 }
		if err := windows.GetFileInformationByHandleEx(h, windows.FileAttributeTagInfo, (*byte)(unsafe.Pointer(&ti)), uint32(unsafe.Sizeof(ti))); err != nil { // #nosec G103 -- structure the API fills
			return entry{}, err
		}
		if ti.tag == windows.IO_REPARSE_TAG_MOUNT_POINT || ti.tag == windows.IO_REPARSE_TAG_SYMLINK {
			e.kind = entryLink
			// Owned by the system or an administrator is not enough: the
			// compatibility junctions in every profile ("Application Data",
			// "Cookies", ...) belong to SYSTEM, but the user has full
			// control of them and could point one at C:\Windows\System32
			// (the owner stays). So nobody else may change, remove or
			// replace the link either. And it must have one name only: a
			// hard link of an administrator's link made elsewhere would carry
			// the same owner and permissions.
			e.trusted = fi.NumberOfLinks == 1 && onlyTrustedMay(h, linkRights) && onlyTrustedMay(windows.Handle(f.Fd()), parentRights)
			t, err := reparseTarget(h)
			if err != nil {
				return entry{}, &os.PathError{Op: "readlink", Path: d.Join(name), Err: err}
			}
			e.target = t
			return e, nil
		}
		// Another kind of reparse point (a cloud file placeholder): what
		// it is, not a link.
	}
	if dir {
		e.kind = entryDir
	} else {
		e.kind = entryOther
	}
	return e, nil
}

// openRelative opens name in the folder dir as it is (a link itself, not
// what it points to), for reading its attributes and owner.
func openRelative(dir windows.Handle, name string) (windows.Handle, error) {
	n, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	oa := &windows.OBJECT_ATTRIBUTES{RootDirectory: dir, ObjectName: n, Attributes: windows.OBJ_CASE_INSENSITIVE}
	oa.Length = uint32(unsafe.Sizeof(*oa)) // #nosec G103 -- size of the structure the API expects
	var h windows.Handle
	var iosb windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&h, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.SYNCHRONIZE, oa, &iosb, nil, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN,
		windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	return h, err
}

// reparseTarget reads where the symbolic link or junction h points: an
// absolute path, or for a relative symbolic link the path relative to its
// folder.
func reparseTarget(h windows.Handle) (string, error) {
	buf := make([]byte, windows.MAXIMUM_REPARSE_DATA_BUFFER_SIZE)
	var n uint32
	if err := windows.DeviceIoControl(h, windows.FSCTL_GET_REPARSE_POINT, nil, 0, &buf[0], uint32(len(buf)), &n, nil); err != nil { // #nosec G115 -- a 16 KiB buffer
		return "", err
	}
	if n < 8 {
		return "", errors.New("short reparse data")
	}
	tag := le32(buf[0:])
	// REPARSE_DATA_BUFFER: tag, length, reserved, then the names' offsets
	// and lengths; a symbolic link has flags before the path buffer.
	off, ln := int(le16(buf[8:])), int(le16(buf[10:]))
	pathStart, relative := 16, false
	switch tag {
	case windows.IO_REPARSE_TAG_SYMLINK:
		pathStart = 20
		relative = le32(buf[16:])&1 != 0 // SYMLINK_FLAG_RELATIVE
	case windows.IO_REPARSE_TAG_MOUNT_POINT:
	default:
		return "", errors.New("not a link")
	}
	s, e := pathStart+off, pathStart+off+ln
	if off%2 != 0 || ln%2 != 0 || e > int(n) || s > e {
		return "", errors.New("bad reparse data")
	}
	u := make([]uint16, ln/2)
	for i := range u {
		u[i] = le16(buf[s+2*i:])
	}
	t := windows.UTF16ToString(u)
	if relative {
		return t, nil
	}
	return ntTarget(t), nil
}

// ntTarget turns the NT path of a link's target into a Win32 path.
func ntTarget(t string) string {
	switch {
	case strings.HasPrefix(t, `\??\UNC\`):
		return `\\` + t[len(`\??\UNC\`):]
	case len(t) >= 6 && strings.HasPrefix(t, `\??\`) && t[5] == ':':
		return t[len(`\??\`):] // \??\C:\...
	case strings.HasPrefix(t, `\??\`):
		// A mounted volume (\??\Volume{GUID}\) and other device paths.
		return `\\?\` + t[len(`\??\`):]
	}
	return t
}

// Rights that let somebody change where a link leads: on the link itself
// (point it elsewhere, remove it, change its permissions or owner) and on
// its folder (remove it, to put another one in its place, or change the
// folder's permissions to be allowed to).
const (
	fileWriteData      = 0x2
	fileAppendData     = 0x4
	fileWriteEA        = 0x10
	fileDeleteChild    = 0x40
	fileWriteAttribute = 0x100
	writeRights        = windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER | windows.GENERIC_WRITE | windows.GENERIC_ALL
	linkRights         = writeRights | fileWriteData | fileAppendData | fileWriteEA | fileWriteAttribute
	parentRights       = fileDeleteChild | windows.WRITE_DAC | windows.WRITE_OWNER | windows.GENERIC_ALL
)

// ownerRights is OWNER RIGHTS (S-1-3-4): the owner, checked separately.
const ownerRights = "S-1-3-4"

// onlyTrustedMay reports whether only the system and administrators may
// do any of rights on h: its owner is one of them, and no entry of its
// access list allows any of rights to anybody else (entries that deny are
// not counted, which errs on the safe side).
func onlyTrustedMay(h windows.Handle, rights uint32) bool {
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !trustedOwner(owner) {
		return false
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil { // no access list: everybody may do everything
		return false
	}
	// ACL header: revision, sbz1, size, AceCount.
	count := le16((*[8]byte)(unsafe.Pointer(dacl))[4:]) // #nosec G103 -- the ACL header
	for i := range uint32(count) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return false
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 || ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if uint32(ace.Mask)&rights == 0 {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return false // another kind of entry that allows: not trusted
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)) // #nosec G103 -- the SID inside the entry
		if trustedOwner(sid) || sid.String() == ownerRights {
			continue
		}
		return false
	}
	return true
}

func le16(b []byte) uint16 { return uint16(b[0]) | uint16(b[1])<<8 }
func le32(b []byte) uint32 { return uint32(le16(b)) | uint32(le16(b[2:]))<<16 }

// rootID identifies the folder r is opened on.
func rootID(r *os.Root) (fileID, error) {
	f, err := r.Open(".")
	if err != nil {
		return fileID{}, err
	}
	defer f.Close() //nolint:errcheck // read-only handle
	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &fi); err != nil {
		return fileID{}, err
	}
	return fileID{fi.VolumeSerialNumber, uint64(fi.FileIndexHigh)<<32 | uint64(fi.FileIndexLow)}, nil
}

// LinkFrom is not used on Windows (file backups record no hard links
// there): the caller restores a copy.
func (d *Dir) LinkFrom(*Dir, string, string, FileID) error { return errors.ErrUnsupported }

// FileID identifies a file (not used here).
type FileID = fileID

// IDOf: hard links are restored as copies here.
func IDOf(*os.File) (FileID, bool) { return FileID{}, false }

// SetMeta sets the preserved Windows attributes (attrs, when setAttrs) and
// the access and modification times (mtime, when not zero) of the entry
// name in d. The entry is opened relative to the folder and as it is: a
// link put at the name gets the attributes itself, nothing it points to.
func (d *Dir) SetMeta(name string, attrs uint32, setAttrs bool, mtime time.Time) error {
	f, err := d.handle()
	if err != nil {
		return err
	}
	n, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return err
	}
	oa := &windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(f.Fd()), ObjectName: n, Attributes: windows.OBJ_CASE_INSENSITIVE}
	oa.Length = uint32(unsafe.Sizeof(*oa)) // #nosec G103 -- size of the structure the API expects
	var h windows.Handle
	var iosb windows.IO_STATUS_BLOCK
	if err := windows.NtCreateFile(&h, windows.FILE_READ_ATTRIBUTES|windows.FILE_WRITE_ATTRIBUTES|windows.SYNCHRONIZE, oa, &iosb, nil, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN,
		windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0); err != nil {
		return &os.PathError{Op: "open", Path: d.Join(name), Err: err}
	}
	defer windows.CloseHandle(h) //nolint:errcheck // closed after the change
	// FILE_BASIC_INFO: zero leaves a time or the attributes as they are.
	var bi struct {
		creation, access, write, change int64
		attrs                           uint32
		_                               uint32
	}
	if !mtime.IsZero() {
		ft := windows.NsecToFiletime(mtime.UnixNano())
		t := int64(ft.HighDateTime)<<32 | int64(ft.LowDateTime)
		bi.access, bi.write = t, t
	}
	if setAttrs {
		var fi windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
			return err
		}
		next := (fi.FileAttributes &^ preservedAttrs) | (attrs & preservedAttrs)
		if next != fi.FileAttributes {
			if next == 0 {
				next = windows.FILE_ATTRIBUTE_NORMAL
			}
			bi.attrs = next
		}
	}
	if bi.attrs == 0 && bi.write == 0 {
		return nil
	}
	if err := windows.SetFileInformationByHandle(h, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&bi)), uint32(unsafe.Sizeof(bi))); err != nil { // #nosec G103 -- structure the API expects
		return &os.PathError{Op: "set attributes", Path: d.Join(name), Err: err}
	}
	return nil
}

// ClearReadOnly removes the read-only attribute of the entry name, so that
// a restored file can replace it.
func (d *Dir) ClearReadOnly(name string) {
	f, err := d.handle()
	if err != nil {
		return
	}
	h, err := openRelative(windows.Handle(f.Fd()), name)
	if err != nil {
		return
	}
	var fi windows.ByHandleFileInformation
	err = windows.GetFileInformationByHandle(h, &fi)
	_ = windows.CloseHandle(h)
	if err != nil || fi.FileAttributes&windows.FILE_ATTRIBUTE_READONLY == 0 || fi.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return
	}
	_ = d.SetMeta(name, fi.FileAttributes&^windows.FILE_ATTRIBUTE_READONLY&preservedAttrs, true, time.Time{})
}

// SetTimes sets the access and modification times of the open file f
// through its handle (not by name).
func SetTimes(f *os.File, t time.Time) error {
	ft := windows.NsecToFiletime(t.UnixNano())
	return windows.SetFileTime(windows.Handle(f.Fd()), nil, &ft, &ft)
}
