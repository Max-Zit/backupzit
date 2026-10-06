//go:build windows

package imaging

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows I/O control codes used for imaging.
const (
	ioctlDiskGetDriveGeometryEx            = 0x000700A0
	ioctlDiskGetDriveLayoutEx              = 0x00070050
	ioctlDiskSetDriveLayoutEx              = 0x0007C054
	ioctlDiskCreateDisk                    = 0x0007C058
	ioctlDiskUpdateProperties              = 0x00070140
	ioctlDiskGetLengthInfo                 = 0x0007405C
	ioctlDiskGetDiskAttributes             = 0x000700F0
	ioctlDiskSetDiskAttributes             = 0x0007C0F4
	ioctlVolumeGetVolumeDiskExtents        = 0x00560000
	ioctlStorageQueryProperty              = 0x002D1400
	fsctlGetVolumeBitmap                   = 0x0009006F
	fsctlGetNtfsVolumeData                 = 0x00090064
	fsctlLockVolume                        = 0x00090018
	fsctlDismountVolume                    = 0x00090020
	fsctlAllowExtendedDasdIO               = 0x00090083
	partitionStyleMBR                      = 0
	partitionStyleGPT                      = 1
	diskAttributeOffline            uint64 = 0x1
	diskAttributeReadOnly           uint64 = 0x2
)

func ioctl(h windows.Handle, code uint32, in []byte, outSize int) ([]byte, error) {
	for {
		out := make([]byte, outSize)
		var n uint32
		var inPtr *byte
		if len(in) > 0 {
			inPtr = &in[0]
		}
		var outPtr *byte
		if outSize > 0 {
			outPtr = &out[0]
		}
		err := windows.DeviceIoControl(h, code, inPtr, uint32(len(in)), outPtr, uint32(outSize), &n, nil)
		if err == windows.ERROR_INSUFFICIENT_BUFFER || err == windows.ERROR_MORE_DATA {
			if outSize >= 64<<20 {
				return nil, err
			}
			outSize *= 2
			continue
		}
		if err != nil {
			return nil, err
		}
		return out[:n], nil
	}
}

func openDevice(path string, write bool) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	access := uint32(windows.GENERIC_READ)
	if write {
		access |= windows.GENERIC_WRITE
	}
	return windows.CreateFile(p, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_EXISTING, 0, 0)
}

func guidString(b []byte) string {
	g := windows.GUID{
		Data1: binary.LittleEndian.Uint32(b[0:4]),
		Data2: binary.LittleEndian.Uint16(b[4:6]),
		Data3: binary.LittleEndian.Uint16(b[6:8]),
	}
	copy(g.Data4[:], b[8:16])
	return strings.Trim(g.String(), "{}")
}

func guidBytes(s string) ([16]byte, error) {
	var out [16]byte
	g, err := windows.GUIDFromString("{" + strings.Trim(s, "{}") + "}")
	if err != nil {
		return out, err
	}
	binary.LittleEndian.PutUint32(out[0:4], g.Data1)
	binary.LittleEndian.PutUint16(out[4:6], g.Data2)
	binary.LittleEndian.PutUint16(out[6:8], g.Data3)
	copy(out[8:], g.Data4[:])
	return out, nil
}

// physicalDrivePath returns \\.\PhysicalDriveN.
func physicalDrivePath(n int) string { return fmt.Sprintf(`\\.\PhysicalDrive%d`, n) }

// ListDisks returns all physical disks with their partitions and volumes.
func ListDisks() ([]Disk, error) {
	vols, err := listVolumes()
	if err != nil {
		return nil, err
	}
	var disks []Disk
	for n := 0; n < 64; n++ {
		h, err := openDevice(physicalDrivePath(n), false)
		if err != nil {
			if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
				continue
			}
			if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
				return nil, fmt.Errorf("reading disks requires administrator rights: %w", err)
			}
			continue
		}
		d, err := readDisk(h, n)
		windows.CloseHandle(h)
		if err != nil {
			return nil, fmt.Errorf("disk %d: %w", n, err)
		}
		for i := range d.Partitions {
			p := &d.Partitions[i]
			for _, v := range vols {
				if v.disk == n && v.offset == p.Offset {
					p.VolumeGUIDPath = v.guidPath
					p.MountPoints = v.mounts
					p.FileSystem = v.fs
					p.Label = v.label
				}
			}
		}
		d.System = systemDisk(d)
		disks = append(disks, d)
	}
	return disks, nil
}

func systemDisk(d Disk) bool {
	win := strings.ToUpper(strings.TrimRight(windowsDir(), `\`))
	for _, p := range d.Partitions {
		for _, m := range p.MountPoints {
			if len(win) >= 2 && strings.EqualFold(strings.TrimRight(m, `\`), win[:2]) {
				return true
			}
		}
	}
	return false
}

func windowsDir() string {
	dir, _ := windows.GetSystemWindowsDirectory()
	return dir
}

func readDisk(h windows.Handle, n int) (Disk, error) {
	d := Disk{Number: n}
	geo, err := ioctl(h, ioctlDiskGetDriveGeometryEx, nil, 256)
	if err != nil {
		return d, fmt.Errorf("geometry: %w", err)
	}
	d.SectorSize = binary.LittleEndian.Uint32(geo[20:24])
	d.Size = binary.LittleEndian.Uint64(geo[24:32])
	d.Model, d.Bus = storageInfo(h)

	lay, err := ioctl(h, ioctlDiskGetDriveLayoutEx, nil, 48+144*16)
	if err != nil {
		return d, fmt.Errorf("layout: %w", err)
	}
	style := binary.LittleEndian.Uint32(lay[0:4])
	count := int(binary.LittleEndian.Uint32(lay[4:8]))
	switch style {
	case partitionStyleMBR:
		d.Style = StyleMBR
		d.MBRSignature = binary.LittleEndian.Uint32(lay[8:12])
	case partitionStyleGPT:
		d.Style = StyleGPT
		d.GPTDiskID = guidString(lay[8:24])
	default:
		d.Style = StyleRaw
	}
	for i := 0; i < count; i++ {
		e := lay[48+i*144 : 48+(i+1)*144]
		p := Partition{
			Offset: binary.LittleEndian.Uint64(e[8:16]),
			Length: binary.LittleEndian.Uint64(e[16:24]),
			Number: int(binary.LittleEndian.Uint32(e[24:28])),
		}
		u := e[32:]
		switch d.Style {
		case StyleMBR:
			p.MBRType = u[0]
			p.Bootable = u[1] != 0
			if p.MBRType == 0 {
				continue // unused slot
			}
		case StyleGPT:
			p.GPTType = guidString(u[0:16])
			p.GPTID = guidString(u[16:32])
			p.GPTAttributes = binary.LittleEndian.Uint64(u[32:40])
			p.Name = syscall.UTF16ToString((*[36]uint16)(unsafe.Pointer(&u[40]))[:])
		}
		if p.Length == 0 {
			continue
		}
		d.Partitions = append(d.Partitions, p)
	}
	sort.Slice(d.Partitions, func(i, j int) bool { return d.Partitions[i].Offset < d.Partitions[j].Offset })
	return d, nil
}

// storageInfo returns "Vendor Product" and the bus kind ("usb", "sd",
// "virtual-file", "removable" or "") from IOCTL_STORAGE_QUERY_PROPERTY.
func storageInfo(h windows.Handle) (model, bus string) {
	q := make([]byte, 12) // PropertyId=StorageDeviceProperty(0), QueryType=PropertyStandardQuery(0)
	out, err := ioctl(h, ioctlStorageQueryProperty, q, 1024)
	if err != nil || len(out) < 28 {
		return "", ""
	}
	// STORAGE_DEVICE_DESCRIPTOR: RemovableMedia at 10, BusType at 28.
	if len(out) >= 32 {
		switch binary.LittleEndian.Uint32(out[28:32]) {
		case 7: // BusTypeUsb
			bus = "usb"
		case 0xC, 0xD: // BusTypeSd, BusTypeMmc
			bus = "sd"
		case 0xF: // BusTypeFileBackedVirtual (a mounted VHD/VHDX or ISO)
			bus = "virtual-file"
		}
	}
	if bus == "" && out[10] != 0 {
		bus = "removable"
	}
	str := func(off uint32) string {
		if off == 0 || int(off) >= len(out) {
			return ""
		}
		end := int(off)
		for end < len(out) && out[end] != 0 {
			end++
		}
		return strings.TrimSpace(string(out[off:end]))
	}
	return strings.TrimSpace(str(binary.LittleEndian.Uint32(out[12:16])) + " " + str(binary.LittleEndian.Uint32(out[16:20]))), bus
}

type volumeInfo struct {
	guidPath string // \\?\Volume{...}\
	disk     int
	offset   uint64
	mounts   []string
	fs       string
	label    string
}

func listVolumes() ([]volumeInfo, error) {
	buf := make([]uint16, windows.MAX_PATH)
	fh, err := windows.FindFirstVolume(&buf[0], uint32(len(buf)))
	if err != nil {
		return nil, fmt.Errorf("enumerate volumes: %w", err)
	}
	defer windows.FindVolumeClose(fh)
	var out []volumeInfo
	for {
		guidPath := windows.UTF16ToString(buf)
		if v, ok := describeVolume(guidPath); ok {
			out = append(out, v)
		}
		if err := windows.FindNextVolume(fh, &buf[0], uint32(len(buf))); err != nil {
			break
		}
	}
	return out, nil
}

func describeVolume(guidPath string) (volumeInfo, bool) {
	v := volumeInfo{guidPath: guidPath, disk: -1}
	h, err := openDevice(strings.TrimSuffix(guidPath, `\`), false)
	if err != nil {
		// Opening a volume for read needs admin; fall back to no access.
		p, _ := windows.UTF16PtrFromString(strings.TrimSuffix(guidPath, `\`))
		h, err = windows.CreateFile(p, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
		if err != nil {
			return v, false
		}
	}
	ext, err := ioctl(h, ioctlVolumeGetVolumeDiskExtents, nil, 256)
	windows.CloseHandle(h)
	if err != nil || len(ext) < 32 || binary.LittleEndian.Uint32(ext[0:4]) != 1 {
		return v, false // spanned/dynamic volumes are not supported
	}
	v.disk = int(binary.LittleEndian.Uint32(ext[8:12]))
	v.offset = binary.LittleEndian.Uint64(ext[16:24])

	names := make([]uint16, 1024)
	var n uint32
	gp, _ := windows.UTF16PtrFromString(guidPath)
	if windows.GetVolumePathNamesForVolumeName(gp, &names[0], uint32(len(names)), &n) == nil {
		start := 0
		for i := 0; i < int(n); i++ {
			if names[i] == 0 {
				if i > start {
					v.mounts = append(v.mounts, windows.UTF16ToString(names[start:i]))
				}
				start = i + 1
			}
		}
	}
	label := make([]uint16, windows.MAX_PATH+1)
	fs := make([]uint16, windows.MAX_PATH+1)
	if windows.GetVolumeInformation(gp, &label[0], uint32(len(label)), nil, nil, nil, &fs[0], uint32(len(fs))) == nil {
		v.label = windows.UTF16ToString(label)
		v.fs = windows.UTF16ToString(fs)
	}
	return v, true
}
