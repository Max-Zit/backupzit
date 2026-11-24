package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/max-zit/backupzit/internal/imaging"
)

func cmdBitLocker(ctx context.Context, args []string) error {
	usage := errors.New("usage: backupzit-agent bitlocker list | unlock --disk N --partition M (key in BACKUPZIT_BITLOCKER_KEY)")
	if len(args) == 0 {
		return usage
	}
	vols, err := imaging.LockedBitLockerVolumes()
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		if len(vols) == 0 {
			fmt.Println("No locked BitLocker volume found.")
		}
		for _, v := range vols {
			fmt.Printf("  disk %d partition %d, %s, locked\n", v.Disk, v.Partition, humanBytes(v.Length))
		}
		return nil
	case "unlock":
		fs := flag.NewFlagSet("bitlocker unlock", flag.ExitOnError)
		disk := fs.Int("disk", -1, "disk of the locked volume")
		part := fs.Int("partition", -1, "partition of the locked volume")
		_ = fs.Parse(args[1:])
		key := os.Getenv("BACKUPZIT_BITLOCKER_KEY")
		if key == "" {
			return errors.New("set BACKUPZIT_BITLOCKER_KEY to the 48-digit recovery key")
		}
		var match []imaging.LockedVolume
		for _, v := range vols {
			if (*disk < 0 || v.Disk == *disk) && (*part < 0 || v.Partition == *part) {
				match = append(match, v)
			}
		}
		switch len(match) {
		case 0:
			return errors.New("no matching locked volume (see bitlocker list)")
		case 1:
		default:
			// Not "the first one found": the key belongs to one volume.
			return fmt.Errorf("%d locked volumes match; choose one with --disk and --partition (see bitlocker list)", len(match))
		}
		v := match[0]
		letter, err := imaging.UnlockBitLocker(ctx, v, key)
		if err != nil {
			return err
		}
		fmt.Printf("disk %d partition %d unlocked as %s\n", v.Disk, v.Partition, letter)
		return nil
	}
	return usage
}
