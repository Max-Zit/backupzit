package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/max-zit/backupzit/internal/hardened"
	"github.com/max-zit/backupzit/internal/imaging"
	"github.com/max-zit/backupzit/internal/repo"
)

// parsePartitionList reads "1,3,4".
func parsePartitionList(s string) ([]int, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []int
	for _, f := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n < 1 {
			return nil, fmt.Errorf("invalid partition number %q", f)
		}
		out = append(out, n)
	}
	return out, nil
}

// parseResizes reads "3=40G,4=600M".
func parseResizes(s string) (map[int]uint64, error) {
	out := map[int]uint64{}
	if strings.TrimSpace(s) == "" {
		return out, nil
	}
	for _, f := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(f, "=")
		n, err := strconv.Atoi(strings.TrimSpace(k))
		if !ok || err != nil {
			return nil, fmt.Errorf("invalid resize %q (e.g. 3=40G)", f)
		}
		size, err := hardened.ParseSize(v)
		if err != nil {
			return nil, err
		}
		if size <= 0 {
			return nil, fmt.Errorf("partition %d: the size must be larger than 0", n)
		}
		out[n] = uint64(size) // #nosec G115 -- ParseSize rejects negative sizes
	}
	return out, nil
}

func cmdImageLayout(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("image-layout", flag.ExitOnError)
	rf := addRepoFlags(fs)
	srcDisk := fs.Int("source-disk", -1, "for a backup of several disks: the number of the backed up disk")
	targetDisk := fs.Int("target-disk", -1, "take the target size from this local disk")
	targetSize := fs.String("target-size", "", "target disk size (e.g. 256G) instead of --target-disk")
	parts := fs.String("partitions", "", "partitions to restore, e.g. 1,3 (default: all backed up)")
	drop := fs.Bool("drop-others", false, "leave partitions that are not restored out of the layout")
	asSource := fs.Bool("as-source", false, "keep the offsets and sizes of the image")
	resize := fs.String("resize", "", "set partition sizes, e.g. 3=40G")
	asJSON := fs.Bool("json", false, "print JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: backupzit-agent image-layout [options] <snapshot-id|latest>")
		fmt.Fprintln(os.Stderr, "Shows the target layout an image restore would use; writes nothing.")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("snapshot is required")
	}
	sel, err := parsePartitionList(*parts)
	if err != nil {
		return err
	}
	sizes, err := parseResizes(*resize)
	if err != nil {
		return err
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
	img := &sn.Images[idx]

	var size uint64
	switch {
	case *targetSize != "":
		s, err := hardened.ParseSize(*targetSize)
		if err != nil {
			return err
		}
		size = uint64(s) // #nosec G115 -- ParseSize rejects negative sizes
	case *targetDisk >= 0:
		disks, err := imaging.ListDisks()
		if err != nil {
			return err
		}
		for _, d := range disks {
			if d.Number == *targetDisk {
				size = d.Size
			}
		}
		if size == 0 {
			return fmt.Errorf("disk %d not found", *targetDisk)
		}
	default:
		size = img.Size
	}

	limits := imaging.ImageLimits(ctx, r, img)
	opts := imaging.LayoutOptions{Restore: sel, DropOthers: *drop}
	if *asSource {
		opts.Mode = imaging.LayoutAsSource
	}
	l, err := imaging.SuggestLayout(img, size, limits, opts)
	if err == nil && len(sizes) > 0 {
		for n, s := range sizes {
			if err = l.Resize(n, s); err != nil {
				break
			}
		}
		if err == nil {
			err = l.Validate(img, limits)
		}
	}
	needed, _ := imaging.NeededDiskSize(img, limits, opts)

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		out := map[string]any{"limits": limits, "needed_disk_size": needed, "layout": l}
		if err != nil {
			out["error"] = err.Error()
		}
		return enc.Encode(out)
	}
	printImageLayout(img, limits, l, size, needed)
	return err
}

func printImageLayout(img *repo.DiskImage, limits map[int]imaging.PartitionLimits, l *imaging.Layout, size, needed uint64) {
	fmt.Printf("Image: disk %d, %s, %s\n", img.Number, humanBytes(img.Size), strings.ToUpper(img.Style))
	fmt.Printf("Target: %s (smallest possible: %s)\n\n", humanBytes(size), humanBytes(needed))
	fmt.Println("  #  original    minimum     used        shrinkable  resizable")
	for _, p := range img.Partitions {
		lim := limits[p.Number]
		res := "yes"
		if !lim.Resizable {
			res = "no: " + lim.Reason
		}
		shrink := "-"
		if lim.ShrinkableTo > 0 {
			shrink = humanBytes(lim.ShrinkableTo)
		}
		fmt.Printf("  %d  %-10s  %-10s  %-10s  %-10s  %s\n", p.Number, humanBytes(p.Length), humanBytes(lim.Min(&p)), humanBytes(lim.UsedBytes), shrink, res)
	}
	if l == nil {
		return
	}
	fmt.Println("\nTarget layout:")
	for _, lp := range l.Partitions {
		what := "restore"
		if !lp.Restore {
			what = "create empty"
		}
		fmt.Printf("  %d  at %-14d %-10s  %s\n", lp.Source, lp.Offset, humanBytes(lp.Length), what)
	}
}
