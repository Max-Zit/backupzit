//go:build windows

package imaging

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows/registry"
)

const offlineSoftwareHive = `BZ_OFFLINE_SOFTWARE`

// FindWindowsInstalls lists the Windows installations on all local disks
// except the running one.
func FindWindowsInstalls(ctx context.Context) ([]WindowsInstall, error) {
	disks, err := ListDisks()
	if err != nil {
		return nil, err
	}
	var out []WindowsInstall
	for i := range disks {
		d := &disks[i]
		if d.System {
			continue
		}
		for j := range d.Partitions {
			p := &d.Partitions[j]
			if p.VolumeGUIDPath == "" {
				continue
			}
			if _, err := os.Stat(p.VolumeGUIDPath + `Windows\System32\config\SYSTEM`); err != nil {
				continue
			}
			wi := WindowsInstall{Disk: d.Number, Partition: p.Number, Style: d.Style, volumeGUIDPath: p.VolumeGUIDPath}
			if sp := findSystemPartition(d, p); sp != nil && sp != p {
				wi.SystemPartition = sp.Number
			} else if sp == p {
				wi.SystemPartition = p.Number
			}
			wi.Product, wi.Version, wi.Build = offlineWindowsVersion(ctx, p.VolumeGUIDPath)
			out = append(out, wi)
		}
	}
	return out, nil
}

// offlineWindowsVersion reads product name and version from the SOFTWARE
// hive of an offline Windows.
func offlineWindowsVersion(ctx context.Context, volumeGUIDPath string) (product, version, build string) {
	win, unmount, err := letterFor(volumeGUIDPath)
	if err != nil {
		return "", "", ""
	}
	defer unmount()
	hive := win + `\Windows\System32\config\SOFTWARE`
	_, _ = run(ctx, "reg.exe", "unload", `HKLM\`+offlineSoftwareHive)
	if _, err := run(ctx, "reg.exe", "load", `HKLM\`+offlineSoftwareHive, hive); err != nil {
		return "", "", ""
	}
	defer func() {
		for i := 0; i < 5; i++ {
			if _, err := run(context.Background(), "reg.exe", "unload", `HKLM\`+offlineSoftwareHive); err == nil {
				return
			}
			time.Sleep(time.Second)
		}
	}()
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, offlineSoftwareHive+`\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
	if err != nil {
		return "", "", ""
	}
	defer k.Close()
	product, _, _ = k.GetStringValue("ProductName")
	version, _, _ = k.GetStringValue("DisplayVersion")
	build, _, _ = k.GetStringValue("CurrentBuild")
	return product, version, build
}

// RepairBoot makes the Windows installation on the given disk and
// partition start again: backups first, then the selected repairs and
// fresh boot files.
func RepairBoot(ctx context.Context, wi WindowsInstall, opts BootRepairOptions, log func(string)) (*BootReport, error) {
	if log == nil {
		log = func(string) {}
	}
	disks, err := ListDisks()
	if err != nil {
		return nil, err
	}
	var disk *Disk
	for i := range disks {
		if disks[i].Number == wi.Disk {
			disk = &disks[i]
		}
	}
	if disk == nil {
		return nil, fmt.Errorf("disk %d not found", wi.Disk)
	}
	if disk.System {
		return nil, fmt.Errorf("disk %d holds the running Windows", wi.Disk)
	}
	var winPart *Partition
	for i := range disk.Partitions {
		if disk.Partitions[i].Number == wi.Partition {
			winPart = &disk.Partitions[i]
		}
	}
	if winPart == nil || winPart.VolumeGUIDPath == "" {
		return nil, fmt.Errorf("partition %d on disk %d has no volume", wi.Partition, wi.Disk)
	}
	if _, err := os.Stat(winPart.VolumeGUIDPath + `Windows\System32\config\SYSTEM`); err != nil {
		return nil, errors.New("no Windows installation on that partition")
	}
	sysPart := findSystemPartition(disk, winPart)

	if opts.BackupDir != "" {
		if err := backupBootState(disk, sysPart, opts.BackupDir, log); err != nil {
			return nil, fmt.Errorf("back up boot data (nothing was changed): %w", err)
		}
	}
	win, unmountWin, err := letterFor(winPart.VolumeGUIDPath)
	if err != nil {
		return nil, fmt.Errorf("mount the Windows volume: %w", err)
	}
	defer unmountWin()

	var warnings []string
	if opts.BootDrivers {
		enabled, err := enableBootDrivers(ctx, win)
		if err != nil {
			warnings = append(warnings, "boot drivers: "+err.Error())
		} else if len(enabled) > 0 {
			log("disk controller drivers set to start at boot: " + strings.Join(enabled, " "))
		}
	}
	if disk.Style == StyleMBR && opts.SetActive && sysPart != nil {
		if err := setActivePartition(ctx, disk.Number, sysPart.Number); err != nil {
			warnings = append(warnings, "set the system partition active: "+err.Error())
		} else {
			log(fmt.Sprintf("partition %d is now active", sysPart.Number))
		}
	}
	if disk.Style == StyleMBR && opts.MBRCode {
		if err := writeMBRCode(ctx, win); err != nil {
			warnings = append(warnings, "write the MBR boot code: "+err.Error())
		} else {
			log("MBR boot code written")
		}
	}

	var br BootReport
	if opts.BootFiles != "" {
		br = writeBootFilesAs(ctx, disk, win, sysPart, strings.ToUpper(opts.BootFiles), log)
	} else {
		br = writeBootFiles(ctx, disk, win, sysPart, log)
	}
	br.Warnings = append(warnings, br.Warnings...)
	return &br, nil
}

// writeBootFilesAs runs bcdboot with a firmware chosen by the user.
func writeBootFilesAs(ctx context.Context, disk *Disk, win string, sysPart *Partition, fw string, log func(string)) BootReport {
	br := BootReport{WindowsVolume: win, Firmware: DetectFirmware(), BootFiles: fw}
	if fw != "UEFI" && fw != "BIOS" && fw != "ALL" {
		br.Warnings = append(br.Warnings, fmt.Sprintf("unknown boot files %q (UEFI, BIOS or ALL)", fw))
		return br
	}
	if sysPart == nil {
		br.Warnings = append(br.Warnings, "no system (boot) partition found; boot files were not rebuilt")
		return br
	}
	sys, unmount, err := letterFor(sysPart.VolumeGUIDPath)
	if err != nil {
		br.Warnings = append(br.Warnings, "mount the system partition: "+err.Error())
		return br
	}
	defer unmount()
	log("boot files for " + fw)
	out, err := runRetry(ctx, "bcdboot.exe", win+`\Windows`, "/s", sys, "/f", fw)
	if err != nil {
		br.Warnings = append(br.Warnings, fmt.Sprintf(`bcdboot %s\Windows /s %s /f %s: %v: %s`, win, sys, fw, err, strings.TrimSpace(out)))
		return br
	}
	br.Rebuilt = true
	return br
}

// backupBootState copies sector 0 (and the first MiB with the GPT) and
// the BCD store of the system partition to dir.
func backupBootState(disk *Disk, sysPart *Partition, dir string, log func(string)) error {
	stamp := time.Now().Format("20060102-150405")
	dst := filepath.Join(dir, fmt.Sprintf("boot-backup-disk%d-%s", disk.Number, stamp))
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	h, err := openDevice(physicalDrivePath(disk.Number), false)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(h), physicalDrivePath(disk.Number))
	head := make([]byte, 1<<20)
	n, err := f.ReadAt(head, 0)
	_ = f.Close()
	if err != nil && n == 0 {
		return fmt.Errorf("read disk start: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dst, "disk-head.bin"), head[:n], 0o600); err != nil {
		return err
	}
	if sysPart != nil && sysPart.VolumeGUIDPath != "" {
		for _, rel := range []string{`EFI\Microsoft\Boot\BCD`, `Boot\BCD`} {
			b, err := os.ReadFile(sysPart.VolumeGUIDPath + rel) // #nosec G304 -- fixed path on the system partition
			if err != nil {
				continue
			}
			name := strings.ReplaceAll(rel, `\`, "_")
			if err := os.WriteFile(filepath.Join(dst, name), b, 0o600); err != nil {
				return err
			}
		}
	}
	log("boot data backed up to " + dst)
	return nil
}

// setActivePartition marks one partition of an MBR disk active.
func setActivePartition(ctx context.Context, disk, part int) error {
	script, err := os.CreateTemp("", "bz-diskpart-*.txt")
	if err != nil {
		return err
	}
	defer os.Remove(script.Name())
	_, _ = fmt.Fprintf(script, "select disk %d\nselect partition %d\nactive\n", disk, part)
	_ = script.Close()
	out, err := run(ctx, "diskpart.exe", "/s", script.Name())
	if err != nil {
		return fmt.Errorf("diskpart: %v: %s", err, strings.TrimSpace(out))
	}
	return nil
}

// writeMBRCode writes the Windows boot code to the MBR of the disk that
// holds volume win (e.g. "F:") with bootsect.exe, which the recovery
// medium carries (it is part of the Windows ADK, not of Windows PE).
func writeMBRCode(ctx context.Context, win string) error {
	exe, err := exec.LookPath("bootsect.exe")
	if err != nil {
		return errors.New("bootsect.exe is not in the recovery environment")
	}
	out, err := run(ctx, exe, "/nt60", win, "/mbr", "/force")
	if err != nil {
		return fmt.Errorf("bootsect: %v: %s", err, strings.TrimSpace(out))
	}
	return nil
}
