//go:build windows

package fsutil

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Pinned holds every folder of a path open, from the volume root down, with
// handles that let nobody else rename, remove or replace them while they
// are held (FILE_SHARE_DELETE is not given). Programs that only take a path
// (PowerShell's New-VHD, the virtual disk API, Hyper-V) then find exactly
// the folders checked here. On the way a link (junction, mount point or
// symbolic link) is followed only when it is trusted, by the rule of
// restores to the original location: the system or an administrator owns
// it, nobody else may change, remove or replace it, and it has one name.
// C:\ClusterStorage\VolumeN and other volume mount points are such links.
type Pinned struct {
	path    string
	h       windows.Handle   // the folder itself
	handles []windows.Handle // every handle held, closed by Close
}

// Path is the folder's path as it was given (through its links).
func (p *Pinned) Path() string { return p.path }

// Close lets the folders go. It may be called more than once.
func (p *Pinned) Close() error {
	for _, h := range p.handles {
		_ = windows.CloseHandle(h)
	}
	p.handles = nil
	return nil
}

const (
	// Little access: what pins is the sharing mode. (Listing is not asked
	// for: system links such as "Documents and Settings" deny it.)
	pinAccess = windows.FILE_TRAVERSE | windows.FILE_READ_ATTRIBUTES | windows.READ_CONTROL | windows.SYNCHRONIZE
	pinShare  = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE // not FILE_SHARE_DELETE
	// Rights that let somebody add, remove or replace what is in a folder.
	folderRights = fileWriteData | fileAppendData | fileDeleteChild | writeRights
	// privateFolder: the system, administrators and Hyper-V administrators
	// (who manage the VMs anyway) have full control; the folder does not
	// inherit what its parent allows others.
	privateFolder = "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;S-1-5-32-578)"
)

// PinFolder pins the folder p; with create, missing folders are created
// (they inherit their permissions, like os.MkdirAll).
func PinFolder(p string, create bool) (*Pinned, error) {
	return pinFolder(filepath.Clean(p), create, 0)
}

func pinFolder(p string, create bool, depth int) (*Pinned, error) {
	if depth > 8 {
		return nil, fmt.Errorf("%s: too many links on the way", p)
	}
	vol := filepath.VolumeName(p)
	if vol == "" || !filepath.IsAbs(p) {
		return nil, fmt.Errorf("%s: not an absolute path", p)
	}
	root := vol + `\`
	r, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(r, pinAccess, pinShare, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, &pathErr{"open", root, err}
	}
	pin := &Pinned{path: root, h: h, handles: []windows.Handle{h}}
	cur := root
	for _, c := range strings.Split(strings.TrimPrefix(p[len(vol):], `\`), `\`) {
		if c == "" || c == "." {
			continue
		}
		if c == ".." {
			_ = pin.Close()
			return nil, fmt.Errorf("%s: .. in the path", p)
		}
		cur = filepath.Join(cur, c)
		if err := pin.step(cur, c, create, depth); err != nil {
			_ = pin.Close()
			return nil, err
		}
	}
	pin.path = p
	return pin, nil
}

// step pins the folder name below the current one; cur is its full path.
func (p *Pinned) step(cur, name string, create bool, depth int) error {
	disp := uint32(windows.FILE_OPEN)
	if create {
		disp = windows.FILE_OPEN_IF
	}
	h, err := ntOpenFolder(p.h, name, disp, nil)
	if err != nil {
		return &pathErr{"open", cur, err}
	}
	p.handles = append(p.handles, h)
	link, err := isLink(h)
	if err != nil {
		return &pathErr{"open", cur, err}
	}
	if !link {
		p.h = h
		return nil
	}
	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
		return err
	}
	if fi.NumberOfLinks != 1 || !onlyTrustedMay(h, linkRights) || !onlyTrustedMay(p.h, parentRights) {
		return fmt.Errorf("%s is a link others may change: it is not followed (choose another folder)", cur)
	}
	t, err := reparseTarget(h)
	if err != nil {
		return &pathErr{"readlink", cur, err}
	}
	if !filepath.IsAbs(t) {
		t = filepath.Join(filepath.Dir(cur), t)
	}
	// The link stays held (it cannot be removed or replaced meanwhile), and
	// so does every folder of its target.
	tp, err := pinFolder(t, false, depth+1)
	if err != nil {
		return err
	}
	p.handles = append(p.handles, tp.handles...)
	p.h = tp.h
	return nil
}

// Sub pins the folder name in p, creating it when create is set (private
// as with CreatePrivate, else with p's inherited permissions); created
// tells whether it was new.
func (p *Pinned) Sub(name string, create, private bool) (sub *Pinned, created bool, err error) {
	if strings.ContainsAny(name, `\/:`) || name == "" || name == "." || name == ".." {
		return nil, false, fmt.Errorf("%q: not a folder name", name)
	}
	if create {
		if sub, err = p.CreatePrivate(name, private); err == nil {
			return sub, true, nil
		}
		if !errors.Is(err, windows.STATUS_OBJECT_NAME_COLLISION) {
			return nil, false, err
		}
	}
	sub = &Pinned{path: p.path, h: p.h}
	if err := sub.step(filepath.Join(p.path, name), name, false, 0); err != nil {
		_ = sub.Close()
		return nil, false, err
	}
	sub.path = filepath.Join(p.path, name)
	return sub, false, nil
}

// CreatePrivate creates and pins the new folder name in p. With private,
// only the system, administrators and Hyper-V administrators may use it
// (it does not inherit p's permissions); otherwise it inherits them. It
// fails with STATUS_OBJECT_NAME_COLLISION if name exists.
func (p *Pinned) CreatePrivate(name string, private bool) (*Pinned, error) {
	if strings.ContainsAny(name, `\/:`) || name == "" || name == "." || name == ".." {
		return nil, fmt.Errorf("%q: not a folder name", name)
	}
	var sd *windows.SECURITY_DESCRIPTOR
	if private {
		var err error
		if sd, err = windows.SecurityDescriptorFromString(privateFolder); err != nil {
			return nil, err
		}
	}
	h, err := ntOpenFolder(p.h, name, windows.FILE_CREATE, sd)
	if err != nil {
		return nil, err
	}
	return &Pinned{path: filepath.Join(p.path, name), h: h, handles: []windows.Handle{h}}, nil
}

// OthersMayChange reports whether anybody but the system, administrators
// and Hyper-V administrators may add, remove or replace entries of the
// folder.
func (p *Pinned) OthersMayChange() bool { return !onlyTrustedMay(p.h, folderRights, hyperVAdmins) }

// hyperVAdmins is BUILTIN\Hyper-V Administrators.
const hyperVAdmins = "S-1-5-32-578"

// ntOpenFolder opens (or creates, by disp) the folder name in dir as it is:
// a link itself, not what it points to.
func ntOpenFolder(dir windows.Handle, name string, disp uint32, sd *windows.SECURITY_DESCRIPTOR) (windows.Handle, error) {
	n, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	oa := &windows.OBJECT_ATTRIBUTES{RootDirectory: dir, ObjectName: n, Attributes: windows.OBJ_CASE_INSENSITIVE, SecurityDescriptor: sd}
	oa.Length = uint32(unsafe.Sizeof(*oa)) // #nosec G103 -- size of the structure the API expects
	var h windows.Handle
	var iosb windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&h, pinAccess, oa, &iosb, nil, windows.FILE_ATTRIBUTE_DIRECTORY, pinShare, disp,
		windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	return h, err
}

// isLink reports whether h is a junction, mount point or symbolic link
// (other reparse points, such as cloud folders, are folders).
func isLink(h windows.Handle) (bool, error) {
	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
		return false, err
	}
	if fi.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		return false, nil
	}
	var ti struct{ attrs, tag uint32 }
	if err := windows.GetFileInformationByHandleEx(h, windows.FileAttributeTagInfo, (*byte)(unsafe.Pointer(&ti)), uint32(unsafe.Sizeof(ti))); err != nil { // #nosec G103 -- structure the API fills
		return false, err
	}
	return ti.tag == windows.IO_REPARSE_TAG_MOUNT_POINT || ti.tag == windows.IO_REPARSE_TAG_SYMLINK, nil
}

type pathErr struct {
	op, path string
	err      error
}

func (e *pathErr) Error() string { return e.op + " " + e.path + ": " + e.err.Error() }
func (e *pathErr) Unwrap() error { return e.err }
