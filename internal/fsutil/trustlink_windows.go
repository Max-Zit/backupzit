//go:build windows

package fsutil

import (
	"io/fs"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// TrustedLink reports whether the symbolic link or junction at path belongs
// to the system or to an administrator: owned by SYSTEM, TrustedInstaller,
// the Administrators group or a member of it. Restores to the original
// location follow only such links. Any user may create a junction (no
// right is needed, unlike symbolic links), so a junction of a user that
// pointed from his profile into C:\Windows\System32 used to make a
// restore by the agent (as SYSTEM) write his backed-up files there.
func TrustedLink(path string, _ fs.FileInfo) bool {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	// The link itself, not what it points to.
	h, err := windows.CreateFile(p, windows.READ_CONTROL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h) //nolint:errcheck // read-only handle
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return false
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return false
	}
	return trustedOwner(owner)
}

// trustedInstaller is NT SERVICE\TrustedInstaller.
const trustedInstaller = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"

func trustedOwner(owner *windows.SID) bool {
	if owner.IsWellKnown(windows.WinLocalSystemSid) || owner.IsWellKnown(windows.WinBuiltinAdministratorsSid) || owner.String() == trustedInstaller {
		return true
	}
	for _, a := range localAdmins() {
		if windows.EqualSid(a, owner) {
			return true
		}
	}
	return false
}

var (
	adminsOnce sync.Once
	admins     []*windows.SID
)

// localAdmins returns the members of the local Administrators group (an
// administrator who creates a link on client Windows owns it himself, not
// the group). The group name depends on the language, so it is looked up
// by its well-known SID.
func localAdmins() []*windows.SID {
	adminsOnce.Do(func() {
		sid, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
		if err != nil {
			return
		}
		name, _, _, err := sid.LookupAccount("")
		if err != nil {
			return
		}
		n, err := windows.UTF16PtrFromString(name)
		if err != nil {
			return
		}
		var buf *byte
		var read, total uint32
		r, _, _ := procNetLocalGroupGetMembers.Call(0, uintptr(unsafe.Pointer(n)), 0, uintptr(unsafe.Pointer(&buf)),
			uintptr(maxPreferredLength), uintptr(unsafe.Pointer(&read)), uintptr(unsafe.Pointer(&total)), 0)
		if r != 0 || buf == nil {
			return
		}
		defer windows.NetApiBufferFree(buf) //nolint:errcheck // frees the buffer of the call above
		members := unsafe.Slice((**windows.SID)(unsafe.Pointer(buf)), read)
		for _, m := range members {
			if c, err := m.Copy(); err == nil {
				admins = append(admins, c)
			}
		}
	})
	return admins
}

// maxPreferredLength is MAX_PREFERRED_LENGTH: the call allocates what it needs.
const maxPreferredLength = 0xFFFFFFFF

var procNetLocalGroupGetMembers = windows.NewLazySystemDLL("netapi32.dll").NewProc("NetLocalGroupGetMembers")
