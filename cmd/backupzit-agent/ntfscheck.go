package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/max-zit/backupzit/internal/imaging/ntfsresize"
)

// cmdNTFSCheck checks the NTFS structures ntfsresize relies on (a
// diagnostic for shrunk partitions, and for sources a shrink refuses).
func cmdNTFSCheck(args []string) error {
	fs := flag.NewFlagSet("ntfs-check", flag.ExitOnError)
	dev := fs.String("device", "", `disk or image, e.g. \\.\PhysicalDrive2`)
	off := fs.Int64("offset", 0, "partition offset in bytes")
	recs := fs.String("records", "", "only describe these file records, e.g. 0,22335,45256")
	_ = fs.Parse(args)
	if *dev == "" {
		return errors.New("--device is required")
	}
	if *off < 0 {
		return errors.New("--offset must not be negative")
	}
	nums, err := parseRecordList(*recs)
	if err != nil {
		return err
	}
	f, err := os.Open(*dev)
	if err != nil {
		return err
	}
	defer f.Close()
	// Raw devices need sector-aligned reads; 4096 suits 512-byte and 4Kn
	// disks alike.
	v, err := ntfsresize.Open(ntfsresize.NewAlignedReader(f, *off, 0, 4096))
	if err != nil {
		return err
	}
	if len(nums) > 0 {
		for _, n := range nums {
			d, err := v.DumpRecord(n)
			if err != nil {
				fmt.Printf("record %d: %v\n", n, err)
				continue
			}
			fmt.Print(d)
		}
		return nil
	}
	fmt.Printf("clusters %d of %d bytes, %d used, last used %d, records of %d bytes\n", v.Clusters, v.ClusterSize, v.UsedClusters, v.LastUsedCluster(), v.RecordSize)
	fmt.Println(v.StateReport())
	if rep, err := v.BitmapReport(); err == nil {
		fmt.Print(rep)
	}
	probs, err := v.Check(40)
	if err != nil {
		return err
	}
	for _, p := range probs {
		fmt.Println("  problem:", p)
	}
	if len(probs) == 0 {
		fmt.Println("no problems found")
	}
	return nil
}

// parseRecordList reads "0,22335,45256": file record numbers (0 is $MFT).
func parseRecordList(s string) ([]uint64, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []uint64
	for _, f := range strings.Split(s, ",") {
		n, err := strconv.ParseUint(strings.TrimSpace(f), 10, 48)
		if err != nil {
			return nil, fmt.Errorf("invalid file record number %q", f)
		}
		out = append(out, n)
	}
	return out, nil
}
