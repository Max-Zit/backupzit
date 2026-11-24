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

	"github.com/max-zit/backupzit/internal/winenc"
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

// PrepareForNewHardware makes a restored Windows installation start on
// different hardware ("universal restore"): it enables the boot start of
// common disk controller drivers in the offline registry, adds drivers from
// driverDirs (INF folders, searched recursively) with DISM and rebuilds the
// boot files with bcdboot.
func PrepareForNewHardware(ctx context.Context, disk DiskRef, driverDirs []string, log func(string)) (*HardwareReport, error) {
	if log == nil {
		log = func(string) {}
	}
	rep := &HardwareReport{}
	d, winPart, err := waitForWindows(ctx, disk, 0)
	if err != nil {
		return nil, err
	}
	sysPart := findSystemPartition(d, winPart)

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
	br := writeBootFiles(ctx, d, win, sysPart, log)
	rep.Firmware, rep.BootFiles, rep.BootRebuilt = br.Firmware, br.BootFiles, br.Rebuilt
	rep.Warnings = append(rep.Warnings, br.Warnings...)
	return rep, nil
}

// waitForWindows waits until Windows has mounted the volumes of a freshly
// written disk and returns the disk and its Windows partition: the one at
// offset at, or the first one with at 0. A disk with the number that is
// not ref's disk is refused at once.
func waitForWindows(ctx context.Context, ref DiskRef, at uint64) (*Disk, *Partition, error) {
	var disk *Disk
	for i := 0; i < 30; i++ {
		disks, err := ListDisks()
		if err != nil {
			return nil, nil, err
		}
		disk, err = ref.Pick(disks)
		if err != nil && ref.ID != nil && disk == nil && hasNumber(disks, ref.Number) {
			return nil, nil, err
		}
		if disk != nil && findWindowsPartition(disk, at) != nil {
			break
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	if disk == nil {
		return nil, nil, fmt.Errorf("disk %d not found after the restore", ref.Number)
	}
	winPart := findWindowsPartition(disk, at)
	if winPart == nil {
		return disk, nil, errors.New("no Windows installation found on the disk")
	}
	return disk, winPart, nil
}

// writeBootFiles runs bcdboot for this machine's firmware (not just the
// partition style: an MBR disk on a UEFI machine needs both).
func writeBootFiles(ctx context.Context, disk *Disk, win string, sysPart *Partition, log func(string)) BootReport {
	br := BootReport{WindowsVolume: win, Firmware: DetectFirmware()}
	fw, warn := BootFilesFor(br.Firmware, disk.Style)
	br.BootFiles = fw
	if warn != "" {
		br.Warnings = append(br.Warnings, warn)
	}
	log("firmware " + firmwareLabel(br.Firmware) + ", boot files for " + fw)
	if sysPart == nil {
		br.Warnings = append(br.Warnings, "no system (boot) partition found; boot files were not rebuilt")
		return br
	}
	sys, unmountSys, err := letterFor(sysPart.VolumeGUIDPath)
	if err != nil {
		br.Warnings = append(br.Warnings, "mount the system partition: "+err.Error())
		return br
	}
	defer unmountSys()
	out, err := runBcdboot(ctx, win, sys, fw)
	if err != nil && fw == "ALL" {
		// An MBR disk on a UEFI machine gets both. If the UEFI part fails
		// on its NTFS system partition, BIOS boot files still let it start
		// in legacy mode.
		br.Warnings = append(br.Warnings, fmt.Sprintf(`bcdboot %s\Windows /s %s /f ALL: %v: %s; writing the BIOS boot files only`, win, sys, err, strings.TrimSpace(out)))
		fw = "BIOS"
		br.BootFiles = fw
		out, err = runBcdboot(ctx, win, sys, fw)
	}
	if err != nil {
		br.Warnings = append(br.Warnings, fmt.Sprintf(`bcdboot %s\Windows /s %s /f %s: %v: %s`, win, sys, fw, err, strings.TrimSpace(out)))
		return br
	}
	br.Rebuilt = true
	return br
}

// runBcdboot writes the boot files of the Windows at win to the system
// partition sys. Outside Windows PE the computer runs its own Windows: the
// new firmware boot entry goes last (/addlast), so the computer keeps
// starting from its own disk and not from the restored one.
func runBcdboot(ctx context.Context, win, sys, fw string) (string, error) {
	args := []string{win + `\Windows`, "/s", sys, "/f", fw}
	if fw != "BIOS" && !inWinPE() {
		args = append(args, "/addlast")
	}
	return runRetry(ctx, "bcdboot.exe", args...)
}

// inWinPE tells whether this is Windows PE (the recovery medium), which
// marks itself with the MiniNT key.
func inWinPE() bool {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\MiniNT`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	_ = k.Close()
	return true
}

// RebuildBoot writes the boot files of the Windows installation at offset
// windowsAt of a disk (0: the first one found) for this machine's
// firmware, e.g. after partitions were moved.
func RebuildBoot(ctx context.Context, disk DiskRef, windowsAt uint64, log func(string)) (*BootReport, error) {
	if log == nil {
		log = func(string) {}
	}
	d, winPart, err := waitForWindows(ctx, disk, windowsAt)
	if err != nil {
		return nil, err
	}
	win, unmountWin, err := letterFor(winPart.VolumeGUIDPath)
	if err != nil {
		return nil, fmt.Errorf("mount the Windows volume: %w", err)
	}
	defer unmountWin()
	br := writeBootFiles(ctx, d, win, findSystemPartition(d, winPart), log)
	return &br, nil
}

// findWindowsPartition returns the partition at offset at (any, with at 0)
// that holds a Windows installation.
func findWindowsPartition(d *Disk, at uint64) *Partition {
	for i := range d.Partitions {
		p := &d.Partitions[i]
		if p.VolumeGUIDPath == "" || at != 0 && p.Offset != at {
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
	if err := windows.GetVolumePathNamesForVolumeName(vol, &buf[0], uint32(len(buf)), &n); err == nil { // #nosec G115 -- length of a small buffer for a Windows API call
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
		return mp[:2], func() { _ = windows.DeleteVolumeMountPoint(mpp) }, nil
	}
	return "", func() {}, errors.New("no free drive letter")
}

const offlineHive = `BZ_OFFLINE_SYSTEM`

// enableBootDrivers loads the restored SYSTEM hive and sets the start type
// of the known disk controller drivers to boot start.
func enableBootDrivers(ctx context.Context, win string) ([]string, error) {
	hive := win + `\Windows\System32\config\SYSTEM`
	_, _ = run(ctx, "reg.exe", "unload", `HKLM\`+offlineHive) // leftover from an interrupted run
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
	_ = sel.Close()
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
		_ = k.Close()
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
	_ = os.MkdirAll(scratch, 0o700)
	out, err := runRetry(ctx, "dism.exe", "/Image:"+win+`\`, "/Add-Driver", "/Driver:"+dir, "/Recurse", "/ScratchDir:"+scratch)
	n := len(dismAddedRe.FindAllString(out, -1))
	if err != nil {
		lines := strings.Split(strings.TrimSpace(out), "\n")
		return n, fmt.Errorf(`dism /Image:%s\ /Add-Driver: %v: %s`, win, err, strings.TrimSpace(lines[len(lines)-1]))
	}
	return n, nil
}

func run(ctx context.Context, name string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(cctx, name, args...).CombinedOutput() // #nosec G204 -- fixed program; arguments are separate values, no shell
	return winenc.Text(out), err
}

// RecoveryDriverDirs returns driver folders shipped with the recovery
// environment (X:\BackupZit\drivers in the recovery ISO).
func RecoveryDriverDirs() []string {
	var dirs []string
	if sd := os.Getenv("SystemDrive"); sd != "" {
		d := filepath.Join(sd+`\`, "BackupZit", "drivers")
		if fi, err := os.Stat(d); err == nil && fi.IsDir() { // #nosec G703 -- driver folder named by the administrator
			dirs = append(dirs, d)
		}
	}
	return dirs
}

// runRetry runs a command up to three times: right after a restore Windows
// may still be mounting the new volumes, and DISM and bcdboot then fail.
func runRetry(ctx context.Context, name string, args ...string) (string, error) {
	var out string
	var err error
	for i := 0; i < 3; i++ {
		if out, err = run(ctx, name, args...); err == nil {
			return out, nil
		}
		select {
		case <-ctx.Done():
			return out, err
		case <-time.After(10 * time.Second):
		}
	}
	if strings.TrimSpace(out) == "" {
		out = "(no output)"
	}
	return out, err
}
