package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/max-zit/backupzit/internal/imaging"
	"github.com/max-zit/backupzit/internal/recovery"
)

func cmdBootRepair(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("boot-repair", flag.ExitOnError)
	list := fs.Bool("list", false, "only list the Windows installations and the firmware")
	disk := fs.Int("disk", -1, "disk with the Windows installation (default: the only one found)")
	part := fs.Int("partition", -1, "partition with the Windows installation")
	files := fs.String("boot-files", "", "force the boot files: UEFI, BIOS or ALL (default: for this machine's firmware)")
	mbr := fs.Bool("mbr", false, "also write the Windows boot code to the MBR (MBR disks; needs bootsect.exe)")
	active := fs.Bool("active", false, "also mark the system partition active (MBR disks)")
	drivers := fs.Bool("boot-drivers", false, "also set the disk controller drivers to start at boot")
	backup := fs.String("backup-dir", "", "where to save sector 0 and the BCD store first (default: current folder)")
	asJSON := fs.Bool("json", false, "print JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: backupzit-agent boot-repair [--list] [--disk N --partition M] [options]")
		fmt.Fprintln(os.Stderr, "Makes an existing Windows installation start again: rebuilds its boot files.")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	fw := imaging.DetectFirmware()
	installs, err := imaging.FindWindowsInstalls(ctx)
	if err != nil {
		return err
	}
	if *list {
		if *asJSON {
			return json.NewEncoder(os.Stdout).Encode(map[string]any{"firmware": fw, "installs": installs})
		}
		fmt.Println("This computer starts in", fw, "mode.")
		if len(installs) == 0 {
			fmt.Println("No Windows installation found.")
		}
		for _, w := range installs {
			fmt.Printf("  disk %d partition %d (%s): %s %s build %s, system partition %d\n",
				w.Disk, w.Partition, strings.ToUpper(w.Style), w.Product, w.Version, w.Build, w.SystemPartition)
		}
		return nil
	}

	var pick []imaging.WindowsInstall
	for _, w := range installs {
		if (*disk < 0 || w.Disk == *disk) && (*part < 0 || w.Partition == *part) {
			pick = append(pick, w)
		}
	}
	switch len(pick) {
	case 0:
		return errors.New("no matching Windows installation found (see --list)")
	case 1:
	default:
		return errors.New("several Windows installations found; choose one with --disk and --partition (see --list)")
	}
	w := pick[0]
	dir := *backup
	if dir == "" {
		// Not the current folder: in Windows PE that is X: (RAM), and it
		// may be on the disk being repaired.
		disks, err := imaging.ListDisks()
		if err != nil {
			return err
		}
		if dir, err = recovery.BootBackupDir(disks, w.Disk, recovery.DriveRoots()); err != nil {
			return fmt.Errorf("%w (or give one with --backup-dir)", err)
		}
	}
	fmt.Printf("repairing the boot of %s on disk %d partition %d\n", w.Product, w.Disk, w.Partition)
	rep, err := imaging.RepairBoot(ctx, w, imaging.BootRepairOptions{
		BootFiles: *files, MBRCode: *mbr, SetActive: *active, BootDrivers: *drivers, BackupDir: dir,
	}, func(s string) { fmt.Println(" ", s) })
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(rep)
	}
	fmt.Printf("boot files for %s written: %v (firmware %s)\n", rep.BootFiles, rep.Rebuilt, rep.Firmware)
	for _, w := range rep.Warnings {
		fmt.Println("  warning:", w)
	}
	if !rep.Rebuilt {
		return errors.New("the boot files were not rebuilt")
	}
	return nil
}
