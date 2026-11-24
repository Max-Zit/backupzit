//go:build windows

package imaging

import (
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var procGetFirmwareType = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetFirmwareType")

// DetectFirmware tells how the running machine booted: GetFirmwareType,
// in Windows PE also PEFirmwareType (1 = BIOS, 2 = UEFI), and the Secure
// Boot state from the registry.
func DetectFirmware() Firmware {
	var fw Firmware
	if procGetFirmwareType.Find() == nil {
		var t uint32
		if r, _, _ := procGetFirmwareType.Call(uintptr(unsafe.Pointer(&t))); r != 0 { // #nosec G103 -- pointer passed to a Windows API call
			fw.Type = firmwareName(uint64(t))
		}
	}
	if fw.Type == FirmwareUnknown {
		if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control`, registry.QUERY_VALUE); err == nil {
			if v, _, err := k.GetIntegerValue("PEFirmwareType"); err == nil {
				fw.Type = firmwareName(v)
			}
			_ = k.Close()
		}
	}
	if fw.Type == FirmwareUEFI {
		if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\SecureBoot\State`, registry.QUERY_VALUE); err == nil {
			if v, _, err := k.GetIntegerValue("UEFISecureBootEnabled"); err == nil {
				fw.SecureBoot = v != 0
			}
			_ = k.Close()
		}
	}
	return fw
}

func firmwareName(t uint64) string {
	switch t {
	case 1:
		return FirmwareBIOS
	case 2:
		return FirmwareUEFI
	}
	return FirmwareUnknown
}
