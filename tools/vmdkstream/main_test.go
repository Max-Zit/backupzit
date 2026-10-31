package main

import (
	"bytes"
	"io"
	"math/rand"
	"testing"
)

func image(size int64) []byte {
	rnd := rand.New(rand.NewSource(size))
	img := make([]byte, size)
	for g := int64(0); g*grainSize*sector < size; g++ {
		start := g * grainSize * sector
		end := min(start+grainSize*sector, size)
		switch g % 3 {
		case 0:
			rnd.Read(img[start:end])
		case 2:
			copy(img[start:end], bytes.Repeat([]byte("backupzit "), int(end-start)/10))
		} // 1: an empty grain
	}
	return img
}

func TestRoundTrip(t *testing.T) {
	for _, size := range []int64{
		sector,                                   // smaller than a grain
		gtEntries * grainSize * sector,           // exactly one grain table
		3*gtEntries*grainSize*sector + 77*sector, // partial last table and grain
	} {
		img := image(size)
		var f bytes.Buffer
		if err := write(&f, bytes.NewReader(img), size, "disk.vmdk", "lsilogic"); err != nil {
			t.Fatal(err)
		}
		if f.Len()%sector != 0 {
			t.Fatalf("size %d: file of %d bytes", size, f.Len())
		}
		var disk bytes.Buffer
		if err := verify(bytes.NewReader(f.Bytes()), &disk); err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		d := disk.Bytes()
		if int64(len(d)) < size || !bytes.Equal(d[:size], img) || !zero(d[size:]) {
			t.Fatalf("size %d: disk differs", size)
		}
	}
}

func TestVerifyRejects(t *testing.T) {
	size := int64(2 * gtEntries * grainSize * sector)
	var f bytes.Buffer
	if err := write(&f, bytes.NewReader(image(size)), size, "disk.vmdk", "lsilogic"); err != nil {
		t.Fatal(err)
	}
	good := f.Bytes()
	cases := map[string][]byte{
		// Like qemu-img: nothing after the last grain.
		"no trailing metadata": good[:len(good)-(1+1+1+1+1)*sector-2*(1+4)*sector],
		"no end-of-stream":     good[:len(good)-sector],
		"data after the end":   append(append([]byte{}, good...), make([]byte, sector)...),
	}
	bad := append([]byte{}, good...)
	bad[(1+1)*sector+40] ^= 0xff // inside the first grain's compressed data
	cases["damaged grain"] = bad
	for name, b := range cases {
		if err := verify(bytes.NewReader(b), io.Discard); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRejectsOddSize(t *testing.T) {
	if err := write(io.Discard, bytes.NewReader(make([]byte, 100)), 100, "d.vmdk", "lsilogic"); err == nil {
		t.Fatal("accepted a size that is not a multiple of a sector")
	}
}
