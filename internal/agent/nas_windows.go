package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/max-zit/backupzit/internal/api"
)

var (
	mpr                       = windows.NewLazySystemDLL("mpr.dll")
	procWNetAddConnection2W   = mpr.NewProc("WNetAddConnection2W")
	procWNetCancelConnection2 = mpr.NewProc("WNetCancelConnection2W")
)

// netResource is NETRESOURCEW.
type netResource struct {
	Scope, Type, DisplayType, Usage          uint32
	LocalName, RemoteName, Comment, Provider *uint16
}

const resourceTypeDisk = 1

// mountNAS connects the SMB share with the job's credentials (for the
// agent's service account only) and returns its UNC path. NFS shares need
// a Linux agent.
func mountNAS(ctx context.Context, s nasShare, cred *api.NASShare, writable bool) (string, func(), error) {
	if s.Proto != "smb" {
		return "", nil, errors.New("NFS shares are backed up through a Linux agent; choose a Linux agent or use SMB")
	}
	unc := `\\` + s.Host + `\` + s.Share
	remote, err := windows.UTF16PtrFromString(unc)
	if err != nil {
		return "", nil, err
	}
	// A connection left by an interrupted run may hold other credentials.
	procWNetCancelConnection2.Call(uintptr(unsafe.Pointer(remote)), 0, 1)
	nr := netResource{Type: resourceTypeDisk, RemoteName: remote}
	var user, pass *uint16
	if cred.User != "" {
		name := cred.User
		if cred.Domain != "" {
			name = cred.Domain + `\` + cred.User
		}
		if user, err = windows.UTF16PtrFromString(name); err != nil {
			return "", nil, err
		}
		if pass, err = windows.UTF16PtrFromString(cred.Password); err != nil {
			return "", nil, err
		}
	}
	if r, _, _ := procWNetAddConnection2W.Call(uintptr(unsafe.Pointer(&nr)), uintptr(unsafe.Pointer(pass)), uintptr(unsafe.Pointer(user)), 0); r != 0 {
		return "", nil, fmt.Errorf("connect %s: %w", unc, windows.Errno(r))
	}
	unmount := func() { procWNetCancelConnection2.Call(uintptr(unsafe.Pointer(remote)), 0, 1) }
	root := unc
	if s.Sub != "" {
		root = filepath.Join(unc, filepath.FromSlash(s.Sub))
	}
	if _, err := os.Stat(root); err != nil {
		unmount()
		return "", nil, fmt.Errorf("%s is not reachable: %w", root, err)
	}
	return root, unmount, nil
}
