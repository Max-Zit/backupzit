package hyperv

import (
	"encoding/binary"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The Virtual Disk API (virtdisk.dll) opens VHD/VHDX files, including the
// read-only parents of a running VM's checkpoint, and attaches them as a
// disk so they can be read and written like a physical drive.
var (
	virtdisk                   = windows.NewLazySystemDLL("virtdisk.dll")
	procOpenVirtualDisk        = virtdisk.NewProc("OpenVirtualDisk")
	procAttachVirtualDisk      = virtdisk.NewProc("AttachVirtualDisk")
	procDetachVirtualDisk      = virtdisk.NewProc("DetachVirtualDisk")
	procGetVirtualDiskPhysPath = virtdisk.NewProc("GetVirtualDiskPhysicalPath")
	procGetVirtualDiskInfo     = virtdisk.NewProc("GetVirtualDiskInformation")
)

const (
	virtualDiskAccessNone      = 0
	virtualDiskAccessAll       = 0x003f0000
	attachFlagReadOnly         = 0x1
	attachFlagNoDriveLetter    = 0x2
	getVirtualDiskInfoSize     = 1
	ioctlDiskSetDiskAttributes = 0x0007C0F4
	ioctlDiskUpdateProperties  = 0x00070140
	diskAttributeOffline       = 0x1
)

type virtualStorageType struct {
	DeviceID uint32
	VendorID windows.GUID
}

// attachedDisk is a VHD/VHDX attached as a local disk.
type attachedDisk struct {
	h    windows.Handle
	path string // \.\PhysicalDriveN
	size uint64
}

func callErr(r uintptr) error {
	if r != 0 {
		return windows.Errno(r)
	}
	return nil
}

// attachVHD opens and attaches a virtual disk file. Read-only attachments
// work on the parents of a running VM's checkpoint.
func attachVHD(file string, readOnly bool) (*attachedDisk, error) {
	if err := procOpenVirtualDisk.Find(); err != nil {
		return nil, fmt.Errorf("Virtual Disk API not available: %w", err)
	}
	p, err := windows.UTF16PtrFromString(file)
	if err != nil {
		return nil, err
	}
	st := virtualStorageType{} // unknown: detected from the file
	var h windows.Handle
	var r uintptr
	if readOnly {
		// OPEN_VIRTUAL_DISK_PARAMETERS version 2: GetInfoOnly, ReadOnly, ResiliencyGuid.
		params := make([]byte, 28)
		binary.LittleEndian.PutUint32(params[0:], 2)
		binary.LittleEndian.PutUint32(params[8:], 1) // ReadOnly
		r, _, _ = procOpenVirtualDisk.Call(uintptr(unsafe.Pointer(&st)), uintptr(unsafe.Pointer(p)), virtualDiskAccessNone, 0,
			uintptr(unsafe.Pointer(&params[0])), uintptr(unsafe.Pointer(&h)))
	} else {
		params := make([]byte, 8) // version 1, RWDepth 1
		binary.LittleEndian.PutUint32(params[0:], 1)
		binary.LittleEndian.PutUint32(params[4:], 1)
		r, _, _ = procOpenVirtualDisk.Call(uintptr(unsafe.Pointer(&st)), uintptr(unsafe.Pointer(p)), virtualDiskAccessAll, 0,
			uintptr(unsafe.Pointer(&params[0])), uintptr(unsafe.Pointer(&h)))
	}
	if err := callErr(r); err != nil {
		return nil, fmt.Errorf("open %s: %w", file, err)
	}
	d := &attachedDisk{h: h}
	ok := false
	defer func() {
		if !ok {
			windows.CloseHandle(h)
		}
	}()
	flags := uintptr(attachFlagNoDriveLetter)
	if readOnly {
		flags |= attachFlagReadOnly
	}
	ap := []uint32{1, 0} // ATTACH_VIRTUAL_DISK_PARAMETERS version 1
	r, _, _ = procAttachVirtualDisk.Call(uintptr(h), 0, flags, 0, uintptr(unsafe.Pointer(&ap[0])), 0)
	if err := callErr(r); err != nil {
		return nil, fmt.Errorf("attach %s: %w", file, err)
	}
	buf := make([]uint16, 260)
	n := uint32(len(buf) * 2)
	r, _, _ = procGetVirtualDiskPhysPath.Call(uintptr(h), uintptr(unsafe.Pointer(&n)), uintptr(unsafe.Pointer(&buf[0])))
	if err := callErr(r); err != nil {
		procDetachVirtualDisk.Call(uintptr(h), 0, 0)
		return nil, fmt.Errorf("disk path of %s: %w", file, err)
	}
	d.path = windows.UTF16ToString(buf)
	// GET_VIRTUAL_DISK_INFO with Version SIZE: VirtualSize at offset 8.
	info := make([]byte, 32)
	binary.LittleEndian.PutUint32(info[0:], getVirtualDiskInfoSize)
	isz := uint32(len(info))
	r, _, _ = procGetVirtualDiskInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&isz)), uintptr(unsafe.Pointer(&info[0])), 0)
	if err := callErr(r); err != nil {
		procDetachVirtualDisk.Call(uintptr(h), 0, 0)
		return nil, fmt.Errorf("size of %s: %w", file, err)
	}
	d.size = binary.LittleEndian.Uint64(info[8:])
	ok = true
	return d, nil
}

func (d *attachedDisk) close() {
	procDetachVirtualDisk.Call(uintptr(d.h), 0, 0)
	windows.CloseHandle(d.h)
}

// openForWrite opens the attached disk for raw writes and takes it offline,
// so Windows does not mount the partitions that appear while writing.
func (d *attachedDisk) openForWrite() (*os.File, error) {
	p, _ := windows.UTF16PtrFromString(d.path)
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return nil, err
	}
	in := make([]byte, 40)
	binary.LittleEndian.PutUint32(in[0:4], 40)
	binary.LittleEndian.PutUint64(in[8:16], diskAttributeOffline)
	binary.LittleEndian.PutUint64(in[16:24], diskAttributeOffline)
	var ret uint32
	if err := windows.DeviceIoControl(h, ioctlDiskSetDiskAttributes, &in[0], uint32(len(in)), nil, 0, &ret, nil); err != nil {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("take disk offline: %w", err)
	}
	return os.NewFile(uintptr(h), d.path), nil
}

// ---- Resilient Change Tracking (RCT)

var procQueryChangesVirtualDisk = virtdisk.NewProc("QueryChangesVirtualDisk")

const getVirtualDiskInfoChangeTrackingState = 15

// rctState returns whether change tracking is on for the opened disk and
// its most recent change tracking ID.
func (d *attachedDisk) rctState() (bool, string, error) {
	info := make([]byte, 1024)
	binary.LittleEndian.PutUint32(info[0:], getVirtualDiskInfoChangeTrackingState)
	isz := uint32(len(info))
	r, _, _ := procGetVirtualDiskInfo.Call(uintptr(d.h), uintptr(unsafe.Pointer(&isz)), uintptr(unsafe.Pointer(&info[0])), 0)
	if err := callErr(r); err != nil {
		return false, "", err
	}
	enabled := binary.LittleEndian.Uint32(info[8:]) != 0
	// MostRecentId is a NUL-terminated UTF-16 string at offset 16.
	var u []uint16
	for i := 16; i+1 < int(isz) && i+1 < len(info); i += 2 {
		c := binary.LittleEndian.Uint16(info[i:])
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return enabled, windows.UTF16ToString(u), nil
}

// rctChanges lists the byte ranges changed since change tracking ID since.
func (d *attachedDisk) rctChanges(since string) ([][2]uint64, error) {
	if err := procQueryChangesVirtualDisk.Find(); err != nil {
		return nil, err
	}
	id, err := windows.UTF16PtrFromString(since)
	if err != nil {
		return nil, err
	}
	var out [][2]uint64
	const batch = 4096
	ranges := make([]uint64, batch*3) // QUERY_CHANGES_VIRTUAL_DISK_RANGE: offset, length, reserved
	var off uint64
	for off < d.size {
		count := uint32(batch)
		var processed uint64
		r, _, _ := procQueryChangesVirtualDisk.Call(uintptr(d.h), uintptr(unsafe.Pointer(id)), uintptr(off), uintptr(d.size-off), 0,
			uintptr(unsafe.Pointer(&ranges[0])), uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&processed)))
		if err := callErr(r); err != nil {
			return nil, err
		}
		for i := 0; i < int(count); i++ {
			out = append(out, [2]uint64{ranges[i*3], ranges[i*3+1]})
		}
		if processed == 0 {
			break
		}
		off += processed
	}
	return out, nil
}

// RCTInfo reports the change tracking state of a virtual disk file and, with
// since, how much changed after that change tracking ID (diagnostics).
func RCTInfo(file, since string) (enabled bool, id string, changed uint64, ranges int, err error) {
	d, err := attachVHD(file, true)
	if err != nil {
		return false, "", 0, 0, err
	}
	defer d.close()
	if enabled, id, err = d.rctState(); err != nil || since == "" {
		return enabled, id, 0, 0, err
	}
	rs, err := d.rctChanges(since)
	for _, r := range rs {
		changed += r[1]
	}
	return enabled, id, changed, len(rs), err
}
