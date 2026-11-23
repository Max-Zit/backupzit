package fsutil

import (
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Functions of macOS's C library that golang.org/x/sys/unix does not wrap
// on darwin: mknodat and mkfifoat (macOS 13, the agent's minimum), which
// create a device node or named pipe relative to an opened folder, so a
// restore keeps creating everything through the opened folder, never by
// path; and the access control list functions, which tell whether a
// folder's ACL lets other users change it. They are called the way
// x/sys/unix calls its own libc functions (a trampoline in
// libc_darwin_*.s, through syscall.syscall6).

//go:cgo_import_dynamic libc_mknodat mknodat "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_mkfifoat mkfifoat "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_acl_get_fd_np acl_get_fd_np "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_acl_to_text acl_to_text "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_acl_free acl_free "/usr/lib/libSystem.B.dylib"

var (
	libc_mknodat_trampoline_addr       uintptr //nolint:revive // name used by the assembly
	libc_mkfifoat_trampoline_addr      uintptr //nolint:revive // name used by the assembly
	libc_acl_get_fd_np_trampoline_addr uintptr //nolint:revive // name used by the assembly
	libc_acl_to_text_trampoline_addr   uintptr //nolint:revive // name used by the assembly
	libc_acl_free_trampoline_addr      uintptr //nolint:revive // name used by the assembly
)

//go:linkname syscall_syscall6 syscall.syscall6
func syscall_syscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno)

// mknodat creates the device node name (mode with its type) in the folder dfd.
func mknodat(dfd int, name string, mode uint32, dev int) error {
	p, err := unix.BytePtrFromString(name)
	if err != nil {
		return err
	}
	// #nosec G103 G115 -- a C string for the libc call, as x/sys/unix passes it; descriptor and device number as the system takes them
	_, _, e := syscall_syscall6(libc_mknodat_trampoline_addr, uintptr(dfd), uintptr(unsafe.Pointer(p)), uintptr(mode), uintptr(dev), 0, 0)
	if e != 0 {
		return e
	}
	return nil
}

// mkfifoat creates the named pipe name in the folder dfd.
func mkfifoat(dfd int, name string, mode uint32) error {
	p, err := unix.BytePtrFromString(name)
	if err != nil {
		return err
	}
	// #nosec G103 G115 -- a C string for the libc call, as x/sys/unix passes it; a descriptor
	_, _, e := syscall_syscall6(libc_mkfifoat_trampoline_addr, uintptr(dfd), uintptr(unsafe.Pointer(p)), uintptr(mode), 0, 0, 0)
	if e != 0 {
		return e
	}
	return nil
}

// aclTypeExtended is ACL_TYPE_EXTENDED of <sys/acl.h>: macOS's own ACLs.
const aclTypeExtended = 0x00000100

// aclText returns the access control list of the open file fd as text
// (acl_to_text: one "name:uuid:id:type:perms" line per entry), "" when it
// has none.
func aclText(fd int) (string, error) {
	acl, _, e := syscall_syscall6(libc_acl_get_fd_np_trampoline_addr, uintptr(fd), aclTypeExtended, 0, 0, 0, 0) // #nosec G115 -- a descriptor
	if acl == 0 {
		// No ACL: the library returns NULL with ENOENT, but Go's runtime
		// only reads errno for a -1 return, so it arrives here as 0.
		if e == 0 || e == syscall.ENOENT {
			return "", nil
		}
		return "", e
	}
	defer syscall_syscall6(libc_acl_free_trampoline_addr, acl, 0, 0, 0, 0, 0) //nolint:errcheck // frees the list
	txt, _, e := syscall_syscall6(libc_acl_to_text_trampoline_addr, acl, 0, 0, 0, 0, 0)
	if txt == 0 {
		return "", e
	}
	defer syscall_syscall6(libc_acl_free_trampoline_addr, txt, 0, 0, 0, 0, 0) //nolint:errcheck // frees the text
	// The C string the library returned, read without converting the
	// integer to a pointer in a way go vet rejects.
	return unix.BytePtrToString(*(**byte)(unsafe.Pointer(&txt))), nil // #nosec G103 -- a C string of libSystem
}

// aclLetsOthersChange reports whether an ACL text has an entry that allows
// adding, deleting or writing entries of a folder: such an entry lets
// another user change it whatever its mode says. Deny entries (Apple puts
// "everyone deny delete" on some folders) grant nothing.
func aclLetsOthersChange(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		f := strings.Split(line, ":")
		if len(f) < 5 || f[len(f)-2] != "allow" {
			continue
		}
		for _, p := range strings.Split(f[len(f)-1], ",") {
			switch strings.TrimSpace(p) {
			case "add_file", "add_subdirectory", "delete_child", "write", "writeattr", "writeextattr", "writesecurity", "chown":
				return true
			}
		}
	}
	return false
}
