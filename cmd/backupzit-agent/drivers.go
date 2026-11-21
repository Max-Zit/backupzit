package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/max-zit/backupzit/internal/imaging"
)

func cmdDrivers(ctx context.Context, args []string) error {
	usage := errors.New("usage: backupzit-agent drivers missing | load <folder> | add --disk N --partition M <folder>")
	if len(args) == 0 {
		return usage
	}
	switch args[0] {
	case "missing":
		devs, err := imaging.ProblemDevices(ctx)
		if err != nil {
			return err
		}
		if len(devs) == 0 {
			fmt.Println("Every device has a working driver.")
		}
		for _, d := range devs {
			fmt.Printf("  %-30s %-14s %s\n    %s\n", d.Description, d.Class, d.Problem, d.InstanceID)
		}
		return nil
	case "load":
		if len(args) != 2 {
			return usage
		}
		res, err := imaging.LoadDrivers(ctx, args[1], func(s string) { fmt.Println(" ", s) })
		if err != nil {
			return err
		}
		fmt.Printf("%d drivers loaded, %d failed. Disks and network cards that needed them are usable now.\n", len(res.Loaded), len(res.Failed))
		for _, f := range res.Failed {
			fmt.Println("  failed:", f)
		}
		return nil
	case "add":
		fs := flag.NewFlagSet("drivers add", flag.ExitOnError)
		disk := fs.Int("disk", -1, "disk with the Windows installation")
		part := fs.Int("partition", -1, "partition with the Windows installation")
		_ = fs.Parse(args[1:])
		if fs.NArg() != 1 {
			return usage
		}
		installs, err := imaging.FindWindowsInstalls(ctx)
		if err != nil {
			return err
		}
		var pick []imaging.WindowsInstall
		for _, w := range installs {
			if (*disk < 0 || w.Disk == *disk) && (*part < 0 || w.Partition == *part) {
				pick = append(pick, w)
			}
		}
		if len(pick) != 1 {
			return fmt.Errorf("%d matching Windows installations; choose one with --disk and --partition (see boot-repair --list)", len(pick))
		}
		n, err := imaging.AddDriversToWindows(ctx, pick[0], fs.Arg(0))
		fmt.Printf("%d drivers added to %s on disk %d partition %d\n", n, pick[0].Product, pick[0].Disk, pick[0].Partition)
		return err
	}
	fmt.Fprintln(os.Stderr, usage)
	return usage
}
