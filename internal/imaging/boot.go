package imaging

import (
	"regexp"
	"strings"
)

// Firmware types of the machine the recovery environment runs on.
const (
	FirmwareUEFI    = "uefi"
	FirmwareBIOS    = "bios"
	FirmwareUnknown = ""
)

// Firmware describes how the running machine boots.
type Firmware struct {
	Type       string `json:"type"`                  // FirmwareUEFI, FirmwareBIOS or "" if unknown
	SecureBoot bool   `json:"secure_boot,omitempty"` // UEFI Secure Boot is on
}

// BootReport describes rebuilt boot files.
type BootReport struct {
	WindowsVolume string   `json:"windows_volume,omitempty"`
	Firmware      Firmware `json:"firmware"`
	BootFiles     string   `json:"boot_files,omitempty"`
	Rebuilt       bool     `json:"rebuilt"`
	Warnings      []string `json:"warnings,omitempty"`
}

// HardwareReport describes what PrepareForNewHardware changed.
type HardwareReport struct {
	WindowsVolume  string   `json:"windows_volume"`
	DriversEnabled []string `json:"drivers_enabled"`
	DriversAdded   int      `json:"drivers_added"`
	DriverSources  []string `json:"driver_sources,omitempty"`
	// Firmware of the machine the restore ran on; BootFiles is the bcdboot
	// firmware chosen for it ("UEFI", "BIOS" or "ALL").
	Firmware    Firmware `json:"firmware"`
	BootFiles   string   `json:"boot_files,omitempty"`
	BootRebuilt bool     `json:"boot_rebuilt"`
	Warnings    []string `json:"warnings,omitempty"`
}

func firmwareLabel(fw Firmware) string {
	switch fw.Type {
	case FirmwareUEFI:
		if fw.SecureBoot {
			return "UEFI (Secure Boot on)"
		}
		return "UEFI"
	case FirmwareBIOS:
		return "BIOS"
	}
	return "unknown"
}

// String is e.g. "UEFI (Secure Boot on)".
func (fw Firmware) String() string { return firmwareLabel(fw) }

// BootFilesFor chooses the bcdboot firmware (/f) for a Windows disk of the
// given partition style on a machine with the given firmware, and says
// when the combination will not start as it is.
func BootFilesFor(fw Firmware, style string) (files, warning string) {
	switch {
	case fw.Type == FirmwareUEFI && style == StyleGPT:
		return "UEFI", ""
	case fw.Type == FirmwareBIOS && style == StyleMBR:
		return "BIOS", ""
	case fw.Type == FirmwareUEFI && style == StyleMBR:
		w := "this computer starts in UEFI mode, but the disk has an MBR partition table: it starts only with legacy boot (CSM) turned on in the firmware settings; restore with a GPT target layout to start in UEFI mode"
		if fw.SecureBoot {
			w += " (legacy boot needs Secure Boot turned off)"
		}
		return "ALL", w
	case fw.Type == FirmwareBIOS && style == StyleGPT:
		return "UEFI", "this computer starts in BIOS (legacy) mode, but the disk has a GPT partition table: switch the firmware to UEFI mode, or restore with an MBR target layout"
	case style == StyleMBR:
		// Firmware unknown: what the partition style suggests.
		return "BIOS", ""
	default:
		return "UEFI", ""
	}
}

// WindowsInstall is a Windows installation found on a local disk.
type WindowsInstall struct {
	Disk      int    `json:"disk"`
	Partition int    `json:"partition"`
	Style     string `json:"style"`
	Product   string `json:"product,omitempty"` // e.g. "Windows Server 2025 Standard"
	Version   string `json:"version,omitempty"` // e.g. "24H2"
	Build     string `json:"build,omitempty"`
	// SystemPartition is the partition with the boot files (EFI system
	// partition or active partition), 0 if none was found.
	SystemPartition int `json:"system_partition,omitempty"`
	volumeGUIDPath  string
}

// BootRepairOptions select the repairs.
type BootRepairOptions struct {
	// BootFiles forces the bcdboot firmware ("UEFI", "BIOS", "ALL");
	// empty chooses it from this machine's firmware.
	BootFiles string
	// MBRCode writes the Windows boot code to sector 0 (MBR disks; needs
	// bootsect.exe in the recovery environment).
	MBRCode bool
	// SetActive marks the system partition active (MBR disks).
	SetActive bool
	// BootDrivers sets the disk controller drivers to start at boot.
	BootDrivers bool
	// BackupDir receives copies of sector 0 and the BCD store before
	// anything is changed.
	BackupDir string
}

// LockedVolume is a BitLocker volume that is locked.
type LockedVolume struct {
	Disk      int    `json:"disk"`
	Partition int    `json:"partition"`
	Length    uint64 `json:"length"`
	guidPath  string
}

// recoveryKeyRe matches a BitLocker recovery password: 8 groups of 6 digits.
var recoveryKeyRe = regexp.MustCompile(`^\d{6}(-\d{6}){7}$`)

// ValidRecoveryKey tells whether s looks like a BitLocker recovery key.
func ValidRecoveryKey(s string) bool { return recoveryKeyRe.MatchString(strings.TrimSpace(s)) }
