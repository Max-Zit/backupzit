package imaging

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// bootStorageDrivers are disk controller drivers that ship with Windows (or
// are commonly added). A restored system boots on a different controller
// only if its driver starts at boot; Windows disables the ones it did not
// need during setup.
var bootStorageDrivers = []string{
	"storahci", "stornvme", "msahci", "pciide", "intelide", "atapi", "iaStorV", "iaStorAVC", "iaStorAC", "iaStorA",
	"LSI_SAS", "LSI_SAS2i", "LSI_SAS3i", "LSI_SSS", "megasas", "megasas2i", "megasas35i", "percsas2i", "percsas3i",
	"SmartPqi", "arcsas", "ADP80XX", "HpSAMD", "storvsc", "pvscsi", "vioscsi", "viostor", "VMSCSI",
}

// HardwareReport describes what PrepareForNewHardware changed.
type HardwareReport struct {
	WindowsVolume  string   `json:"windows_volume"`
	DriversEnabled []string `json:"drivers_enabled"`
	DriversAdded   int      `json:"drivers_added"`
	DriverSources  []string `json:"driver_sources,omitempty"`
	BootRebuilt    bool     `json:"boot_rebuilt"`
	Warnings       []string `json:"warnings,omitempty"`
}

// PrepareForNewHardware makes a restored Windows installation start on
// different hardware ("universal restore"): it enables the boot start of
// common disk controller drivers in the offline registry, adds drivers from
// driverDirs (INF folders, searched recursively) with DISM and rebuilds the
// boot files with bcdboot.
func PrepareForNewHardware(ctx context.Context, diskNumber int, driverDirs []string, log func(string)) (*HardwareReport, error) {
	if log == nil {
		log = func(string) {}
	}
	rep := &HardwareReport{}
	// Wait for Windows to mount the restored volumes.
	var disk *Disk
	for i := 0; i < 30; i++ {
		disks, err := ListDisks()
		if err != nil {
			return nil, err
		}
		for j := range disks {
			if disks[j].Number == diskNumber {
				disk = &disks[j]
			}
		}
		if disk != nil && findWindowsPartition(disk) != nil {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	if disk == nil {
		return nil, fmt.Errorf("disk %d not found after the restore", diskNumber)
	}
	winPart := findWindowsPartition(disk)
	if winPart == nil {
		return nil, errors.New("no Windows installation found on the restored disk")
	}
	sysPart := findSystemPartition(disk, winPart)

	win, unmountWin, err := letterFor(winPart.VolumeGUIDPath)
	if err != nil {
		return nil, fmt.Errorf("mount the Windows volume: %w", err)
	}
	defer unmountWin()
	rep.WindowsVolume = win
	log("Windows found on " + win)

	// 1. Disk controller drivers start at boot.
	enabled, err := enableBootDrivers(ctx, win)
	if err != nil {
		rep.Warnings = append(rep.Warnings, "registry: "+err.Error())
	}
	rep.DriversEnabled = enabled

	// 2. Additional drivers.
	for _, dir := range driverDirs {
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("driver folder %s not found", dir))
			continue
		}
		log("adding drivers from " + dir)
		n, err := addDrivers(ctx, win, dir)
		rep.DriversAdded += n
		rep.DriverSources = append(rep.DriverSources, dir)
		if err != nil {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("drivers from %s: %v", dir, err))
		}
	}

	// 3. Boot files for this machine's firmware.
	if sysPart != nil {
		sys, unmountSys, err := letterFor(sysPart.VolumeGUIDPath)
		if err != nil {
			rep.Warnings = append(rep.Warnings, "mount the system partition: "+err.Error())
		} else {
			fw := "UEFI"
			if disk.Style == StyleMBR {
				fw = "BIOS"
			}
			out, err := run(ctx, "bcdboot.exe", win+`\Windows`, "/s", sys, "/f", fw)
			unmountSys()
			if err != nil {
				rep.Warnings = append(rep.Warnings, "bcdboot: "+strings.TrimSpace(out))
			} else {
				rep.BootRebuilt = true
			}
		}
	} else {
		rep.Warnings = append(rep.Warnings, "no system (boot) partition found; boot files were not rebuilt")
	}
	return rep, nil
}

func findWindowsPartition(d *Disk) *Partition {
	for i := range d.Partitions {
		p := &d.Partitions[i]
		if p.VolumeGUIDPath == "" {
			continue
		}
		if _, err := os.Stat(p.VolumeGUIDPath + `Windows\System32\config\SYSTEM`); err == nil {
			return p
		}
	}
	return nil
}

// findSystemPartition returns the EFI system partition (GPT) or the active
// partition (MBR), falling back to the Windows partition itself.
func findSystemPartition(d *Disk, win *Partition) *Partition {
	for i := range d.Partitions {
		p := &d.Partitions[i]
		if d.Style == StyleGPT && strings.EqualFold(p.GPTType, "C12A7328-F81F-11D2-BA4B-00A0C93EC93B") && p.VolumeGUIDPath != "" {
			return p
		}
		if d.Style == StyleMBR && p.Bootable && p.VolumeGUIDPath != "" {
			return p
		}
	}
	if d.Style == StyleMBR {
		return win
	}
	return nil
}

// letterFor gives a volume a temporary drive letter (or returns the one it
// has) and returns "X:" plus a function that removes a letter it added.
func letterFor(volumeGUIDPath string) (string, func(), error) {
	if volumeGUIDPath == "" {
		return "", func() {}, errors.New("volume has no GUID path")
	}
	vol, _ := windows.UTF16PtrFromString(volumeGUIDPath)
	buf := make([]uint16, 1024)
	var n uint32
	if err := windows.GetVolumePathNamesForVolumeName(vol, &buf[0], uint32(len(buf)), &n); err == nil {
		for _, name := range strings.Split(windows.UTF16ToString(buf[:n]), "\x00") {
			if len(name) == 3 && name[1] == ':' {
				return name[:2], func() {}, nil
			}
		}
	}
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return "", func() {}, err
	}
	for l := 'Z'; l >= 'G'; l-- {
		if mask&(1<<uint(l-'A')) != 0 {
			continue
		}
		mp := string(l) + `:\`
		mpp, _ := windows.UTF16PtrFromString(mp)
		if err := windows.SetVolumeMountPoint(mpp, vol); err != nil {
			continue
		}
		return mp[:2], func() { windows.DeleteVolumeMountPoint(mpp) }, nil
	}
	return "", func() {}, errors.New("no free drive letter")
}

const offlineHive = `BZ_OFFLINE_SYSTEM`

// enableBootDrivers loads the restored SYSTEM hive and sets the start type
// of the known disk controller drivers to boot start.
func enableBootDrivers(ctx context.Context, win string) ([]string, error) {
	hive := win + `\Windows\System32\config\SYSTEM`
	run(ctx, "reg.exe", "unload", `HKLM\`+offlineHive) // leftover from an interrupted run
	if out, err := run(ctx, "reg.exe", "load", `HKLM\`+offlineHive, hive); err != nil {
		return nil, fmt.Errorf("load %s: %s", hive, strings.TrimSpace(out))
	}
	defer func() {
		for i := 0; i < 5; i++ {
			if _, err := run(context.Background(), "reg.exe", "unload", `HKLM\`+offlineHive); err == nil {
				return
			}
			time.Sleep(time.Second)
		}
	}()
	sel, err := registry.OpenKey(registry.LOCAL_MACHINE, offlineHive+`\Select`, registry.QUERY_VALUE)
	if err != nil {
		return nil, err
	}
	cur, _, err := sel.GetIntegerValue("Current")
	sel.Close()
	if err != nil {
		return nil, err
	}
	services := fmt.Sprintf(`%s\ControlSet%03d\Services`, offlineHive, cur)
	var enabled []string
	for _, name := range bootStorageDrivers {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, services+`\`+name, registry.QUERY_VALUE|registry.SET_VALUE)
		if err != nil {
			continue // driver not present in this Windows
		}
		changed := false
		if start, _, err := k.GetIntegerValue("Start"); err != nil || start != 0 {
			if k.SetDWordValue("Start", 0) == nil {
				changed = true
			}
		}
		k.Close()
		// Windows 8 and newer disable unused drivers per control set here.
		if registry.DeleteKey(registry.LOCAL_MACHINE, services+`\`+name+`\StartOverride`) == nil {
			changed = true
		}
		if changed {
			enabled = append(enabled, name)
		}
	}
	return enabled, nil
}

var dismAddedRe = regexp.MustCompile(`(?i)installing\s+\d+\s+of\s+\d+`)

// addDrivers adds all drivers below dir to the offline Windows image.
func addDrivers(ctx context.Context, win, dir string) (int, error) {
	scratch := filepath.Join(os.TempDir(), "bz-dism")
	os.MkdirAll(scratch, 0o700)
	out, err := run(ctx, "dism.exe", "/Image:"+win+`\`, "/Add-Driver", "/Driver:"+dir, "/Recurse", "/ScratchDir:"+scratch)
	n := len(dismAddedRe.FindAllString(out, -1))
	if err != nil {
		lines := strings.Split(strings.TrimSpace(out), "\n")
		return n, fmt.Errorf("%s", strings.TrimSpace(lines[len(lines)-1]))
	}
	return n, nil
}

func run(ctx context.Context, name string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(cctx, name, args...).CombinedOutput()
	return string(out), err
}

// RecoveryDriverDirs returns driver folders shipped with the recovery
// environment (X:\BackupZit\drivers in the recovery ISO).
func RecoveryDriverDirs() []string {
	var dirs []string
	if sd := os.Getenv("SystemDrive"); sd != "" {
		d := filepath.Join(sd+`\`, "BackupZit", "drivers")
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			dirs = append(dirs, d)
		}
	}
	return dirs
}
