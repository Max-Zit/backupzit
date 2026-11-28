package agent

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The agent's folder (C:\ProgramData\backupzit) holds agent.json, the
// pending enrollment, the scripts and updates the service runs as SYSTEM.
// Every user may create folders in ProgramData, and junctions need no
// privilege: a user who made the folder (or a junction to a folder of his
// own) before the agent was installed would keep it. protectDir does not
// help then: on a junction it protects the link object only, and the
// agent's reads and writes follow the link into the user's folder; a plain
// folder gets SYSTEM as owner, but a planted agent.json (another console)
// in it would still be loaded (HIGH 6). So before anything in it is read
// or written, the folder is checked: a real folder owned by SYSTEM or
// Administrators is used as it is (every existing installation: the
// service and the installer create it as SYSTEM, and protectDir has made
// SYSTEM its owner since 0.56); anything else is renamed aside unread and
// a new folder is made, protected from its creation on.

// configDirSDDL is the access of the agent's folder, as protectDir sets it.
const configDirSDDL = "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"

// dirState is what checkDir found at a path.
type dirState int

const (
	dirMissing   dirState = iota
	dirTrusted            // a real folder owned by SYSTEM or Administrators
	dirUntrusted          // a link, a file, or a folder of someone else
)

// trustedOwner reports whether a folder with this owner may be used:
// SYSTEM, or Administrators (a folder made from an elevated command line).
// Tests also accept their own account.
var trustedOwner = func(sid *windows.SID) bool {
	return sid.IsWellKnown(windows.WinLocalSystemSid) || sid.IsWellKnown(windows.WinBuiltinAdministratorsSid)
}

// trustDir makes sure dir is a real folder owned by SYSTEM or
// Administrators. A missing folder is created with the protected access
// list; anything else at the path is renamed to dir.untrusted-<time>
// without reading what is in it, and a new folder is made. It returns the
// name the old one got ("" when nothing was set aside).
func trustDir(dir string) (aside string, err error) {
	for range 3 {
		st, why, err := checkDir(dir)
		if err != nil {
			return aside, fmt.Errorf("check the agent's folder %s: %w", dir, err)
		}
		switch st {
		case dirTrusted:
			return aside, nil
		case dirMissing:
			err := createProtectedDir(dir)
			if err == nil || !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
				return aside, err
			}
			// Someone made it in between: look at it again.
		case dirUntrusted:
			name, err := setAside(dir)
			if err != nil {
				return aside, fmt.Errorf("%s is not the agent's own folder (%s) and could not be set aside: %w; remove it (rmdir) and start the agent service again", dir, why, err)
			}
			if aside == "" {
				aside = name
			}
		}
	}
	return aside, fmt.Errorf("%s keeps being replaced by another user; the agent does not use it", dir)
}

// checkDir looks at the object at path itself, without following a
// junction or symbolic link, and through one handle, so that the
// attributes and the owner belong to the same object.
func checkDir(path string) (st dirState, why string, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, "", err
	}
	h, err := windows.CreateFile(p, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	switch {
	case errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND):
		return dirMissing, "", nil
	case errors.Is(err, windows.ERROR_ACCESS_DENIED) && windows.GetCurrentProcessToken().IsElevated():
		// The agent's folder is open to SYSTEM and Administrators: one
		// that shuts them out was made by someone else.
		return dirUntrusted, "it does not let administrators in", nil
	case err != nil:
		return 0, "", err
	}
	defer windows.CloseHandle(h) //nolint:errcheck
	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
		return 0, "", err
	}
	if fi.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return dirUntrusted, "it is a link (junction or symbolic link)", nil
	}
	if fi.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return dirUntrusted, "it is not a folder", nil
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return 0, "", err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return 0, "", err
	}
	if owner == nil || !trustedOwner(owner) {
		return dirUntrusted, "its owner is " + accountName(owner) + ", not SYSTEM or Administrators", nil
	}
	return dirTrusted, "", nil
}

// accountName names a SID for messages.
func accountName(sid *windows.SID) string {
	if sid == nil {
		return "unknown"
	}
	if acc, dom, _, err := sid.LookupAccount(""); err == nil {
		if dom != "" {
			return dom + `\` + acc
		}
		return acc
	}
	return sid.String()
}

// setAside renames path to path.untrusted-<time>. A rename moves a
// junction or symbolic link itself, not the folder it points to, and
// nothing in it is opened.
func setAside(path string) (string, error) {
	from, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	base := path + ".untrusted-" + time.Now().Format("20060102-150405")
	for i := 0; ; i++ {
		name := base
		if i > 0 {
			name += "-" + strconv.Itoa(i)
		}
		to, err := windows.UTF16PtrFromString(name)
		if err != nil {
			return "", err
		}
		// No MOVEFILE_REPLACE_EXISTING: an existing name is never replaced.
		err = windows.MoveFileEx(from, to, 0)
		if err == nil {
			return name, nil
		}
		if i >= 9 || !(errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS)) {
			return "", err
		}
	}
}

// createProtectedDir creates dir with the agent's access list from the
// start: created first and protected after, the folder would for a moment
// let every user create files and junctions in it (ProgramData's
// inherited access). The owner is the creator: SYSTEM for the service and
// the installer, Administrators from an elevated command line.
func createProtectedDir(dir string) error {
	sd, err := windows.SecurityDescriptorFromString(configDirSDDL)
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd} // #nosec G103 -- size of a Windows API structure
	if err := windows.CreateDirectory(p, sa); err != nil {
		return &os.PathError{Op: "mkdir", Path: dir, Err: err}
	}
	return nil
}

// reparsePoint reports whether path itself is a junction, symbolic link
// or another reparse point (GetFileAttributes does not follow it).
func reparsePoint(path string) (bool, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	attrs, err := windows.GetFileAttributes(p)
	if err != nil {
		return false, err
	}
	return attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0, nil
}
