package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/backupzit/backupzit/internal/repo"
	"github.com/backupzit/backupzit/internal/restorer"
	"github.com/backupzit/backupzit/internal/sysbackup"
)

// cmdSystemBackup backs up the whole Linux system (all local file systems
// and the disk layout) for bare-metal restore.
func cmdSystemBackup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("system-backup", flag.ExitOnError)
	rf := addRepoFlags(fs)
	var excludes multiFlag
	fs.Var(&excludes, "exclude", "name pattern to exclude (repeatable)")
	fs.Parse(args)
	r, err := rf.open(ctx)
	if err != nil {
		return err
	}
	defer r.Close()
	last := time.Now()
	sn, err := sysbackup.Backup(ctx, r, sysbackup.BackupOptions{Excludes: excludes, Version: version,
		Progress: func(p string, s *repo.SnapshotStats) {
			if time.Since(last) > 5*time.Second {
				last = time.Now()
				fmt.Printf("  %d files, %s\n", s.Files, humanBytes(s.Bytes))
			}
		}})
	if err != nil {
		return err
	}
	if _, err := r.KeepImmutable(ctx, sn); err != nil {
		return fmt.Errorf("extend immutability: %w", err)
	}
	fmt.Printf("system snapshot %s saved\n", sn.ID.Short())
	printLayout(sn.System)
	st := sn.Stats
	fmt.Printf("  files: %d (%d new, %d changed), %s read, %s new data\n", st.Files, st.FilesNew, st.FilesChanged, humanBytes(st.BytesRead), humanBytes(st.BytesAdded))
	for _, e := range st.Errors {
		fmt.Println("  warning:", e)
	}
	return nil
}

func printLayout(l *repo.SystemLayout) {
	if l == nil {
		return
	}
	boot := "BIOS"
	if l.UEFI {
		boot = "UEFI"
	}
	fmt.Printf("  %s (%s), %s boot\n", l.OS, l.Hostname, boot)
	for _, d := range l.Disks {
		fmt.Printf("  disk %s %s, %s\n", d.Name, humanBytes(d.Size), strings.ToUpper(d.Table))
		for _, p := range d.Partitions {
			fmt.Printf("    %-3d %10s  %-9s %s\n", p.Number, humanBytes(p.Size), p.Content, p.Type)
		}
	}
	for _, f := range l.FileSystems {
		fmt.Printf("  %-12s %-5s %s %s\n", f.MountPoint, f.Type, f.Device, f.UUID)
	}
	for _, v := range l.VGs {
		fmt.Printf("  LVM volume group %s\n", v.Name)
	}
}

func cmdSystemShow(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("system-show", flag.ExitOnError)
	rf := addRepoFlags(fs)
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: backupzit-agent system-show [options] <snapshot-id|latest>")
	}
	r, err := rf.open(ctx)
	if err != nil {
		return err
	}
	defer r.Close()
	sn, err := r.LoadSnapshot(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	if sn.System == nil {
		return fmt.Errorf("snapshot %s is not a system backup", sn.ID.Short())
	}
	fmt.Printf("system snapshot %s from %s\n", sn.ID.Short(), sn.Time.Local().Format("2006-01-02 15:04"))
	printLayout(sn.System)
	return nil
}

func cmdSystemRestore(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("system-restore", flag.ExitOnError)
	rf := addRepoFlags(fs)
	target := fs.String("target", "", "empty disk, logical volume or image file to restore onto, e.g. /dev/sdb")
	disk := fs.String("disk", "", "source disk of the backup (default: the system disk)")
	confirm := fs.Bool("yes-erase-target", false, "confirm that everything on the target is destroyed")
	initramfs := fs.Bool("rebuild-initramfs", false, "rebuild the initramfs with all drivers (dracut systems moved to new hardware)")
	newHW := fs.Bool("new-hardware", false, "adapt to a different machine or VM: network settings bound to MAC addresses, initramfs")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: backupzit-agent system-restore [options] --target DEVICE <snapshot-id|latest>")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if fs.NArg() != 1 || *target == "" {
		fs.Usage()
		return errors.New("snapshot and --target are required")
	}
	if !*confirm {
		return fmt.Errorf("this erases %s completely; add --yes-erase-target to continue", *target)
	}
	r, err := rf.open(ctx)
	if err != nil {
		return err
	}
	defer r.Close()
	sn, err := r.LoadSnapshot(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	last := time.Now()
	res, err := sysbackup.Restore(ctx, r, sn, sysbackup.RestoreOptions{Target: *target, Disk: *disk, RebuildInitramfs: *initramfs, NewHardware: *newHW,
		Log: func(s string) { fmt.Println(" ", s) },
		Progress: func(p string, s *restorer.Stats) {
			if time.Since(last) > 5*time.Second {
				last = time.Now()
				fmt.Printf("    %d files, %s\n", s.Files, humanBytes(s.Bytes))
			}
		}})
	if res != nil {
		for _, f := range res.FileSystems {
			fmt.Println("  file system", f)
		}
		fmt.Printf("  %d files, %s restored; %s\n", res.Files, humanBytes(res.Bytes), res.Boot)
		for _, n := range res.Notes {
			fmt.Println("  note:", n)
		}
		for _, e := range res.Errors {
			fmt.Println("  error:", e)
		}
	}
	if err != nil {
		return err
	}
	fmt.Printf("system %s restored onto %s\n", sn.ID.Short(), *target)
	return nil
}
