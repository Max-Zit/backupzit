package main

import (
	"context"
	"errors"
	"time"

	"encoding/json"
	"flag"
	"fmt"
	"github.com/max-zit/backupzit/internal/repo"
	"os"
	"path/filepath"
	"strings"

	"github.com/max-zit/backupzit/internal/imaging"
)

func cmdDisks(args []string) error {
	fs := flag.NewFlagSet("disks", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	fs.Parse(args)
	disks, err := imaging.ListDisks()
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(disks)
	}
	for _, d := range disks {
		sys := ""
		if d.System {
			sys = "  [system disk]"
		}
		fmt.Printf("Disk %d: %s, %s, %s, sector %d%s\n", d.Number, strings.TrimSpace(d.Model), humanBytes(d.Size), strings.ToUpper(d.Style), d.SectorSize, sys)
		for _, p := range d.Partitions {
			fmt.Printf("  %d  %10s  at %-12d %s\n", p.Number, humanBytes(p.Length), p.Offset, p.Describe())
		}
	}
	return nil
}

func parseInts(s string) ([]int, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []int
	for _, f := range strings.Split(s, ",") {
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(f), "%d", &n); err != nil {
			return nil, fmt.Errorf("invalid number %q", f)
		}
		out = append(out, n)
	}
	return out, nil
}

func progressPrinter(verb string) func(done, total uint64) {
	last := time.Now()
	return func(done, total uint64) {
		if time.Since(last) > 3*time.Second || done == total {
			last = time.Now()
			pct := 100.0
			if total > 0 {
				pct = float64(done) * 100 / float64(total)
			}
			fmt.Printf("  %s %5.1f%%  %s / %s\n", verb, pct, humanBytes(done), humanBytes(total))
		}
	}
}

func cmdImageBackup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("image-backup", flag.ExitOnError)
	rf := addRepoFlags(fs)
	disk := fs.Int("disk", -1, "disk number to image (see: backupzit-agent disks)")
	parts := fs.String("partitions", "", "comma-separated partition numbers (default: all)")
	noVSS := fs.Bool("no-vss", false, "read volumes live instead of from VSS snapshots")
	allDisks := fs.Bool("all-disks", false, "image every internal disk (not USB/SD disks or mounted VHDs) into one snapshot")
	fs.Parse(args)
	if *allDisks {
		*disk = imaging.AllDisks
	} else if *disk < 0 {
		fs.Usage()
		return errors.New("--disk or --all-disks is required")
	}
	sel, err := parseInts(*parts)
	if err != nil {
		return err
	}
	r, err := rf.open(ctx)
	if err != nil {
		return err
	}
	defer r.Close()
	sn, err := imaging.Backup(ctx, r, imaging.BackupOptions{
		Disk: *disk, Partitions: sel, VSS: !*noVSS, Version: version, Progress: progressPrinter("imaged"),
	})
	if err != nil {
		return err
	}
	fmt.Printf("image snapshot %s saved\n", sn.ID.Short())
	if _, err := r.KeepImmutable(ctx, sn); err != nil {
		return fmt.Errorf("extend immutability: %w", err)
	}
	for _, img := range sn.Images {
		printImage(img)
	}
	st := sn.Stats
	fmt.Printf("  data: %s read, %s new (%s stored), %s\n", humanBytes(st.BytesRead), humanBytes(st.BytesAdded), humanBytes(st.BytesStored), st.Duration.Round(time.Second))
	if len(st.Errors) > 0 {
		for _, e := range st.Errors {
			fmt.Println("  warning:", e)
		}
		return fmt.Errorf("image backup finished with %d warnings", len(st.Errors))
	}
	return nil
}

func printImage(img repo.DiskImage) {
	fmt.Printf("  disk %d: %s, %s, %s\n", img.Number, img.Model, humanBytes(img.Size), strings.ToUpper(img.Style))
	for _, p := range img.Partitions {
		state := "layout only"
		if p.Included {
			state = fmt.Sprintf("%s, %s, %s stored", p.Source, p.Method, humanBytes(p.StoredBytes))
		}
		label := strings.Join(p.MountPoints, " ")
		if p.Label != "" {
			label += fmt.Sprintf(" %q", p.Label)
		}
		fmt.Printf("    %d %10s %-6s %-20s %s\n", p.Number, humanBytes(p.Length), p.FileSystem, strings.TrimSpace(label), state)
	}
}

func cmdImageRestore(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("image-restore", flag.ExitOnError)
	rf := addRepoFlags(fs)
	target := fs.Int("target-disk", -1, "disk number to overwrite")
	confirm := fs.Bool("yes-erase-target", false, "confirm that all data on the target disk will be destroyed")
	keepOffline := fs.Bool("keep-offline", false, "leave the target disk offline (when the source disk is in the same machine)")
	srcDisk := fs.Int("source-disk", -1, "for a backup of several disks: the number of the backed up disk to restore")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: backupzit-agent image-restore [options] <snapshot-id|latest>")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if fs.NArg() != 1 || *target < 0 {
		fs.Usage()
		return errors.New("snapshot and --target-disk are required")
	}
	if !*confirm {
		return fmt.Errorf("this erases disk %d completely; add --yes-erase-target to continue", *target)
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
	if len(sn.Images) == 0 {
		return fmt.Errorf("snapshot %s is not an image backup", sn.ID.Short())
	}
	idx, err := imageIndex(sn, *srcDisk)
	if err != nil {
		return err
	}
	fmt.Printf("restoring disk %d of image %s from %s to disk %d\n", sn.Images[idx].Number, sn.ID.Short(), sn.Time.Local().Format("2006-01-02 15:04:05"), *target)
	st, err := imaging.Restore(ctx, r, sn, imaging.RestoreOptions{Image: idx, TargetDisk: *target, KeepOffline: *keepOffline, Progress: progressPrinter("written")})
	if err != nil {
		return err
	}
	fmt.Printf("restored %d partitions, %s written in %s\n", st.Partitions, humanBytes(st.BytesWritten), st.Duration.Round(time.Second))
	return nil
}

// openImageVolume loads snapshot ref and opens the NTFS volume of partition part.
func openImageVolume(ctx context.Context, r *repo.Repository, ref string, disk, part int) (*repo.Snapshot, *repo.PartitionImage, *imaging.Volume, error) {
	sn, err := r.LoadSnapshot(ctx, ref)
	if err != nil {
		return nil, nil, nil, err
	}
	idx, err := imageIndex(sn, disk)
	if err != nil {
		return nil, nil, nil, err
	}
	for i := range sn.Images[idx].Partitions {
		p := &sn.Images[idx].Partitions[i]
		if p.Number == part {
			v, err := imaging.OpenVolume(ctx, r, p)
			return sn, p, v, err
		}
	}
	return nil, nil, nil, fmt.Errorf("image has no partition %d", part)
}

func cmdImageLs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("image-ls", flag.ExitOnError)
	rf := addRepoFlags(fs)
	part := fs.Int("partition", -1, "partition number")
	disk := fs.Int("disk", -1, "for a backup of several disks: the number of the backed up disk")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: backupzit-agent image-ls [options] --partition N <snapshot|latest> [\path]`)
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if fs.NArg() < 1 || *part < 0 {
		fs.Usage()
		return errors.New("snapshot and --partition are required")
	}
	r, err := rf.open(ctx)
	if err != nil {
		return err
	}
	defer r.Close()
	_, _, v, err := openImageVolume(ctx, r, fs.Arg(0), *disk, *part)
	if err != nil {
		return err
	}
	dir := `\`
	if fs.NArg() > 1 {
		dir = fs.Arg(1)
	}
	entries, err := v.List(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		kind := "f"
		size := humanBytes(uint64(e.Size))
		if e.IsDir {
			kind, size = "d", ""
		}
		fmt.Printf("%s %10s  %s  %s\n", kind, size, e.ModTime.Local().Format("2006-01-02 15:04"), e.Name)
	}
	return nil
}

func cmdImageExtract(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("image-extract", flag.ExitOnError)
	rf := addRepoFlags(fs)
	part := fs.Int("partition", -1, "partition number")
	disk := fs.Int("disk", -1, "for a backup of several disks: the number of the backed up disk")
	target := fs.String("target", "", "folder to restore into (the path inside the partition is recreated below it)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: backupzit-agent image-extract [options] --partition N --target DIR <snapshot|latest> \path [\path...]`)
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if fs.NArg() < 2 || *part < 0 || *target == "" {
		fs.Usage()
		return errors.New("snapshot, paths, --partition and --target are required")
	}
	r, err := rf.open(ctx)
	if err != nil {
		return err
	}
	defer r.Close()
	_, _, v, err := openImageVolume(ctx, r, fs.Arg(0), *disk, *part)
	if err != nil {
		return err
	}
	st, err := v.Extract(ctx, fs.Args()[1:], func(p string) string {
		return filepath.Join(*target, filepath.FromSlash(strings.ReplaceAll(p, `\`, "/")))
	}, nil)
	if err != nil {
		return err
	}
	fmt.Printf("extracted %d files, %d folders, %s in %s\n", st.Files, st.Dirs, humanBytes(st.Bytes), st.Duration.Round(time.Millisecond))
	for _, e := range st.Errors {
		fmt.Println("  error:", e)
	}
	if len(st.Errors) > 0 {
		return fmt.Errorf("%d errors", len(st.Errors))
	}
	return nil
}

// cmdPrepareHardware prepares an already restored Windows disk for
// different hardware (the same step as "Restore to different hardware").
func cmdPrepareHardware(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("prepare-hardware", flag.ExitOnError)
	disk := fs.Int("disk", -1, "disk number with the restored Windows (see: backupzit-agent disks)")
	var drivers multiFlag
	fs.Var(&drivers, "drivers", "folder with drivers (.inf) to add (repeatable)")
	fs.Parse(args)
	if *disk < 0 {
		fs.Usage()
		return errors.New("--disk is required")
	}
	dirs := append(imaging.RecoveryDriverDirs(), drivers...)
	rep, err := imaging.PrepareForNewHardware(ctx, *disk, dirs, func(s string) { fmt.Println(" ", s) })
	if err != nil {
		return err
	}
	fmt.Printf("Windows on %s: %d disk controller drivers enabled (%s), %d drivers added, boot files rebuilt: %v\n",
		rep.WindowsVolume, len(rep.DriversEnabled), strings.Join(rep.DriversEnabled, " "), rep.DriversAdded, rep.BootRebuilt)
	for _, w := range rep.Warnings {
		fmt.Println("  warning:", w)
	}
	return nil
}

// imageIndex finds the backed up disk number disk in sn (-1: the only disk
// of a single-disk backup).
func imageIndex(sn *repo.Snapshot, disk int) (int, error) {
	if len(sn.Images) == 0 {
		return 0, fmt.Errorf("snapshot %s is not an image backup", sn.ID.Short())
	}
	if disk < 0 {
		if len(sn.Images) > 1 {
			nums := make([]string, len(sn.Images))
			for i, img := range sn.Images {
				nums[i] = fmt.Sprint(img.Number)
			}
			return 0, fmt.Errorf("snapshot %s holds disks %s; choose one with --source-disk (restore) or --disk (ls, extract)", sn.ID.Short(), strings.Join(nums, ", "))
		}
		return 0, nil
	}
	for i, img := range sn.Images {
		if img.Number == disk {
			return i, nil
		}
	}
	return 0, fmt.Errorf("snapshot %s has no disk %d", sn.ID.Short(), disk)
}
