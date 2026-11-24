// Command cloudfiles makes Windows cloud files placeholders, the kind of
// file OneDrive leaves when Files On-Demand keeps a file "online-only", without
// a OneDrive account: it registers a sync root of its own through the Cloud
// Files API (cldapi.dll) and creates placeholders in it. Lab tool for the
// agent's handling of such files; not shipped.
//
//	cloudfiles make  C:\cftest     sync root with online-only, downloaded, dehydrated and plain files
//	cloudfiles stat  C:\cftest     what Go and Windows report for each file, and a live read
//	cloudfiles backup C:\cftest C:\cfrepo   archiver run (VSS) into a local repository, then the stored content
//	cloudfiles clean C:\cftest     unregisters the sync root and deletes the folder
//
// No sync provider stays connected, so reading an online-only file behaves as
// when OneDrive is not running (its user is not signed in).
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/max-zit/backupzit/internal/archiver"
	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
)

var (
	cldapi                   = windows.NewLazySystemDLL("cldapi.dll")
	procRegisterSyncRoot     = cldapi.NewProc("CfRegisterSyncRoot")
	procUnregisterSyncRoot   = cldapi.NewProc("CfUnregisterSyncRoot")
	procCreatePlaceholders   = cldapi.NewProc("CfCreatePlaceholders")
	procConvertToPlaceholder = cldapi.NewProc("CfConvertToPlaceholder")
	procDehydratePlaceholder = cldapi.NewProc("CfDehydratePlaceholder")
)

// cfSyncRegistration is CF_SYNC_REGISTRATION.
type cfSyncRegistration struct {
	StructSize             uint32
	ProviderName           *uint16
	ProviderVersion        *uint16
	SyncRootIdentity       uintptr
	SyncRootIdentityLength uint32
	FileIdentity           uintptr
	FileIdentityLength     uint32
	ProviderID             windows.GUID
}

// cfSyncPolicies is CF_SYNC_POLICIES.
type cfSyncPolicies struct {
	StructSize            uint32
	HydrationPrimary      uint16
	HydrationModifier     uint16
	PopulationPrimary     uint16
	PopulationModifier    uint16
	InSync                uint32
	HardLink              uint32
	PlaceholderManagement uint32
}

// cfPlaceholderCreateInfo is CF_PLACEHOLDER_CREATE_INFO.
type cfPlaceholderCreateInfo struct {
	RelativeFileName   *uint16
	CreationTime       int64
	LastAccessTime     int64
	LastWriteTime      int64
	ChangeTime         int64
	FileAttributes     uint32
	_                  uint32
	FileSize           int64
	FileIdentity       uintptr
	FileIdentityLength uint32
	Flags              uint32
	Result             int32
	CreateUsn          int64
}

const (
	cfHydrationFull         = 2
	cfPopulationAlwaysFull  = 3
	cfCreateFlagMarkInSync  = 0x2
	cfConvertFlagMarkInSync = 0x1
	cfRegisterFlagMarkRoot  = 0x4
)

var identity = []byte("backupzit-lab")

func hr(r uintptr, what string) error {
	if int32(r) < 0 { // #nosec G115 -- an HRESULT is a signed 32-bit value
		return fmt.Errorf("%s: HRESULT 0x%08X: %v", what, uint32(r), windows.Errno(r&0xFFFF)) // #nosec G115 -- see above
	}
	return nil
}

func register(dir string) error {
	root, _ := windows.UTF16PtrFromString(dir)
	name, _ := windows.UTF16PtrFromString("BackupZit lab")
	ver, _ := windows.UTF16PtrFromString("1.0")
	reg := cfSyncRegistration{
		ProviderName:           name,
		ProviderVersion:        ver,
		SyncRootIdentity:       uintptr(unsafe.Pointer(&identity[0])),
		SyncRootIdentityLength: uint32(len(identity)),
		ProviderID:             windows.GUID{Data1: 0x6b7a1c55, Data2: 0x2f3e, Data3: 0x4b8a, Data4: [8]byte{0x9a, 0x11, 0x5e, 0x42, 0x0b, 0x7c, 0x3d, 0x61}},
	}
	reg.StructSize = uint32(unsafe.Sizeof(reg))
	pol := cfSyncPolicies{HydrationPrimary: cfHydrationFull, PopulationPrimary: cfPopulationAlwaysFull}
	pol.StructSize = uint32(unsafe.Sizeof(pol))
	r, _, _ := procRegisterSyncRoot.Call(uintptr(unsafe.Pointer(root)), uintptr(unsafe.Pointer(&reg)), uintptr(unsafe.Pointer(&pol)), cfRegisterFlagMarkRoot)
	return hr(r, "CfRegisterSyncRoot")
}

// online creates name as an online-only placeholder of size bytes.
func online(dir, name string, size int64, mtime time.Time) error {
	p, _ := windows.UTF16PtrFromString(name)
	ft := windows.NsecToFiletime(mtime.UnixNano())
	t := int64(ft.HighDateTime)<<32 | int64(ft.LowDateTime)
	info := cfPlaceholderCreateInfo{
		RelativeFileName:   p,
		CreationTime:       t,
		LastAccessTime:     t,
		LastWriteTime:      t,
		ChangeTime:         t,
		FileAttributes:     windows.FILE_ATTRIBUTE_NORMAL,
		FileSize:           size,
		FileIdentity:       uintptr(unsafe.Pointer(&identity[0])),
		FileIdentityLength: uint32(len(identity)),
		Flags:              cfCreateFlagMarkInSync,
	}
	base, _ := windows.UTF16PtrFromString(dir)
	var done uint32
	r, _, _ := procCreatePlaceholders.Call(uintptr(unsafe.Pointer(base)), uintptr(unsafe.Pointer(&info)), 1, 0, uintptr(unsafe.Pointer(&done)))
	if err := hr(r, "CfCreatePlaceholders "+name); err != nil {
		return err
	}
	return hr(uintptr(info.Result), "placeholder "+name)
}

func openRW(path string) (windows.Handle, error) {
	p, _ := windows.UTF16PtrFromString(path)
	return windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
}

// downloaded writes data to path and turns it into an in-sync placeholder:
// what OneDrive leaves after a file was downloaded ("available on this
// device"). With dehydrate, its data is then dropped again ("Free up
// space"), keeping its times.
func downloaded(path string, data []byte, dehydrate bool) error {
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	h, err := openRW(path)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	var usn int64
	r, _, _ := procConvertToPlaceholder.Call(uintptr(h), uintptr(unsafe.Pointer(&identity[0])), uintptr(len(identity)), cfConvertFlagMarkInSync, uintptr(unsafe.Pointer(&usn)), 0)
	if err := hr(r, "CfConvertToPlaceholder "+path); err != nil {
		return err
	}
	if !dehydrate {
		return nil
	}
	r, _, _ = procDehydratePlaceholder.Call(uintptr(h), 0, ^uintptr(0), 0, 0)
	return hr(r, "CfDehydratePlaceholder "+path)
}

func content(i int) []byte {
	return bytes.Repeat([]byte(fmt.Sprintf("lab file %d, real content. ", i)), 4000)
}

func cmdMake(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := register(dir); err != nil {
		return err
	}
	old := time.Date(2025, 3, 16, 10, 0, 0, 0, time.UTC)
	for i := 1; i <= 3; i++ {
		if err := online(dir, fmt.Sprintf("online-%d.docx", i), 110070, old); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "plain.txt"), content(0), 0o600); err != nil {
		return err
	}
	if err := downloaded(filepath.Join(dir, "downloaded.txt"), content(1), false); err != nil {
		return err
	}
	if err := downloaded(filepath.Join(dir, "freed.txt"), content(2), true); err != nil {
		return err
	}
	return nil
}

func attrs(path string) (uint32, uint32, error) {
	var fd windows.Win32finddata
	p, _ := windows.UTF16PtrFromString(path)
	h, err := windows.FindFirstFile(p, &fd)
	if err != nil {
		return 0, 0, err
	}
	_ = windows.FindClose(h)
	return fd.FileAttributes, fd.Reserved0, nil
}

func cmdStat(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		fmt.Printf("== %s\n", e.Name())
		if fi, err := os.Lstat(p); err != nil {
			fmt.Printf("   Lstat: %v\n", err)
		} else {
			fmt.Printf("   Lstat: mode=%v regular=%v irregular=%v size=%d\n", fi.Mode(), fi.Mode().IsRegular(), fi.Mode()&os.ModeIrregular != 0, fi.Size())
		}
		if a, tag, err := attrs(p); err != nil {
			fmt.Printf("   attrs: %v\n", err)
		} else {
			fmt.Printf("   attrs=0x%08X reparse=%v tag=0x%08X recallOnData=%v recallOnOpen=%v offline=%v pinned=%v unpinned=%v\n", a,
				a&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0, tag, a&0x400000 != 0, a&0x40000 != 0, a&windows.FILE_ATTRIBUTE_OFFLINE != 0, a&0x80000 != 0, a&0x100000 != 0)
		}
		start := time.Now()
		b, err := readSome(p)
		fmt.Printf("   live read: %d bytes, err=%v, %s, zeros=%v\n", len(b), err, time.Since(start).Round(time.Millisecond), allZero(b))
	}
	return nil
}

func readSome(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 1<<20))
}

func allZero(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

func cmdBackup(dir, repoDir string) error {
	ctx := context.Background()
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		return err
	}
	be, err := backend.OpenLocal(repoDir)
	if err != nil {
		return err
	}
	r, err := repo.Init(ctx, be)
	if err != nil {
		return err
	}
	start := time.Now()
	sn, err := archiver.Run(ctx, r, archiver.Options{Paths: []string{dir}, Hostname: "lab", Version: "lab", VSS: true, NoParent: true})
	if err != nil {
		return err
	}
	fmt.Printf("backup done in %s: files=%d bytes=%d read=%d errors=%d vss=%v\n", time.Since(start).Round(time.Millisecond), sn.Stats.Files, sn.Stats.Bytes, sn.Stats.BytesRead, len(sn.Stats.Errors), sn.VSSVolumes)
	fmt.Printf("cloud-only: files=%d bytes=%d folders=%+v\n", sn.Stats.CloudOnlyFiles, sn.Stats.CloudOnlyBytes, sn.Stats.CloudOnly)
	for _, e := range sn.Stats.Errors {
		fmt.Printf("   error: %s\n", e)
	}
	return walk(ctx, r, sn.Tree, "")
}

func walk(ctx context.Context, r *repo.Repository, id repo.ID, prefix string) error {
	t, err := r.LoadTree(ctx, id)
	if err != nil {
		return err
	}
	sort.Slice(t.Nodes, func(i, j int) bool { return t.Nodes[i].Name < t.Nodes[j].Name })
	for _, n := range t.Nodes {
		p := prefix + "/" + n.Name
		switch {
		case n.Type == repo.NodeDir && n.Subtree != nil:
			if err := walk(ctx, r, *n.Subtree, p); err != nil {
				return err
			}
		case n.Type == repo.NodeFile:
			var data []byte
			for _, b := range n.Content {
				blob, err := r.LoadBlob(ctx, repo.DataBlob, b)
				if err != nil {
					return err
				}
				data = append(data, blob...)
			}
			sum := sha256.Sum256(data)
			fmt.Printf("   stored %s: size=%d content=%d zeros=%v attrs=0x%08X sha=%x\n", p, n.Size, len(data), allZero(data), n.WinAttrs, sum[:6])
		default:
			fmt.Printf("   stored %s: type=%v\n", p, n.Type)
		}
	}
	return nil
}

func cmdClean(dir string) error {
	root, _ := windows.UTF16PtrFromString(dir)
	r, _, _ := procUnregisterSyncRoot.Call(uintptr(unsafe.Pointer(root)))
	uerr := hr(r, "CfUnregisterSyncRoot")
	return errors.Join(uerr, os.RemoveAll(dir))
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: cloudfiles make|stat|clean DIR, or cloudfiles backup DIR REPODIR")
		os.Exit(2)
	}
	dir, err := filepath.Abs(os.Args[2])
	if err == nil {
		switch strings.ToLower(os.Args[1]) {
		case "make":
			err = cmdMake(dir)
		case "stat":
			err = cmdStat(dir)
		case "backup":
			if len(os.Args) < 4 {
				err = errors.New("backup needs DIR REPODIR")
			} else {
				err = cmdBackup(dir, os.Args[3])
			}
		case "clean":
			err = cmdClean(dir)
		default:
			err = fmt.Errorf("unknown command %q", os.Args[1])
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "cloudfiles:", err)
		os.Exit(1)
	}
}
