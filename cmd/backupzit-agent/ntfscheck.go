package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/max-zit/backupzit/internal/imaging/ntfsresize"
)

// offsetReader reads a partition from a disk device or image file.
type offsetReader struct {
	f   *os.File
	off int64
}

func (o offsetReader) ReadAt(p []byte, off int64) (int, error) {
	// Raw devices need sector-aligned reads.
	const sec = 4096
	start := (o.off + off) / sec * sec
	end := (o.off + off + int64(len(p)) + sec - 1) / sec * sec
	buf := make([]byte, end-start)
	n, err := o.f.ReadAt(buf, start)
	skip := int(o.off + off - start)
	if n < skip {
		return 0, io.EOF
	}
	c := copy(p, buf[skip:n])
	if c < len(p) && err == nil {
		err = io.EOF
	}
	return c, err
}

// cmdNTFSCheck checks the NTFS structures ntfsresize relies on (a
// diagnostic for shrunk partitions).
func cmdNTFSCheck(args []string) error {
	fs := flag.NewFlagSet("ntfs-check", flag.ExitOnError)
	dev := fs.String("device", "", `disk or image, e.g. \.\PhysicalDrive2`)
	off := fs.Int64("offset", 0, "partition offset in bytes")
	_ = fs.Parse(args)
	if *dev == "" {
		return errors.New("--device is required")
	}
	f, err := os.Open(*dev)
	if err != nil {
		return err
	}
	defer f.Close()
	v, err := ntfsresize.Open(offsetReader{f: f, off: *off})
	if err != nil {
		return err
	}
	fmt.Printf("clusters %d of %d bytes, %d used, last used %d, records of %d bytes\n", v.Clusters, v.ClusterSize, v.UsedClusters, v.LastUsedCluster(), v.RecordSize)
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
