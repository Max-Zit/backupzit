package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/max-zit/backupzit/internal/hardened"
	"github.com/max-zit/backupzit/internal/imaging"
	"github.com/max-zit/backupzit/internal/recovery"
)

func cmdPartitionRestore(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("partition-restore", flag.ExitOnError)
	rf := addRepoFlags(fs)
	srcDisk := fs.Int("source-disk", -1, "for a backup of several disks: the number of the backed up disk")
	part := fs.Int("partition", 0, "partition of the image to restore")
	target := fs.Int("target-disk", -1, "disk to write to; its other partitions stay")
	replace := fs.Int("replace", 0, "partition of the target disk to overwrite (default: into free space)")
	offset := fs.String("offset", "", "into free space: where the partition starts (e.g. 120G; default: the first space that fits)")
	size := fs.String("size", "", "size of the partition (default: that of the replaced partition, or of the image partition)")
	verify := fs.Bool("verify", false, "read and check every block of the partition before writing")
	confirm := fs.Bool("yes-overwrite", false, "confirm that the replaced partition is overwritten")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: backupzit-agent partition-restore --partition N --target-disk D [--replace P | --offset X] [--size S] <snapshot-id|latest>")
		fmt.Fprintln(os.Stderr, "Writes one partition of a disk image to a disk; the other partitions of that disk stay as they are.")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if fs.NArg() != 1 || *part < 1 || *target < 0 {
		fs.Usage()
		return errors.New("snapshot, --partition and --target-disk are required")
	}
	if *replace > 0 && !*confirm {
		return fmt.Errorf("this overwrites partition %d of disk %d; add --yes-overwrite to continue", *replace, *target)
	}
	into := &recovery.PartitionTarget{Partition: *part, Replace: *replace}
	for _, x := range []struct {
		s   string
		dst *uint64
	}{{*offset, &into.Offset}, {*size, &into.Length}} {
		if x.s == "" {
			continue
		}
		v, err := hardened.ParseSize(x.s)
		if err != nil {
			return err
		}
		*x.dst = uint64(v) // #nosec G115 -- ParseSize rejects negative sizes
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
	idx, err := imageIndex(sn, *srcDisk)
	if err != nil {
		return err
	}
	disks, err := imaging.ListDisks()
	if err != nil {
		return err
	}
	job := recovery.Job{Snapshot: sn, Image: idx, Into: into, TargetDisk: *target, Verify: *verify,
		Protect: append([]string{rf.location}, recovery.BootMediumPaths()...)}
	for _, d := range disks {
		if d.Number == *target {
			id := d.Identity()
			job.TargetID = &id
		}
	}
	if err := job.Check(disks, recovery.ProtectedDisks(disks, job.Protect)); err != nil {
		return err
	}
	fmt.Printf("restoring partition %d of image %s to disk %d\n", *part, sn.ID.Short(), *target)
	written := progressPrinter("written")
	verified := progressPrinter("verified")
	res, err := recovery.Run(ctx, &recovery.Session{Repo: r}, job, func(phase string, done, total uint64) {
		switch phase {
		case "verify":
			verified(done, total)
		case "write":
			written(done, total)
		}
	}, recovery.NewLogger(nil, func(s string) { fmt.Println(" ", s) }))
	if err != nil {
		return err
	}
	fmt.Printf("restored, %s written in %s\n", humanBytes(res.Restore.BytesWritten), res.Restore.Duration.Round(time.Second))
	for _, w := range res.Warnings {
		fmt.Println("  warning:", w)
	}
	if res.Incomplete {
		return exitCodeError{code: recovery.ExitIncomplete, msg: "the data was written, but finishing the disk failed (see the warnings above): it may not start yet"}
	}
	return nil
}
