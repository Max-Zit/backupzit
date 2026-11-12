// This harness is compiled inside isolated copies of BackupZit. It exercises
// the real diskimg.StoreAreas, repository, encryption and local backend.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/diskimg"
	"github.com/max-zit/backupzit/internal/repo"
)

const MiB = 1 << 20
const password = "synthetic-benchmark-only"

var ctx = context.Background()

func must(err error) {
	if err != nil {
		panic(err)
	}
}

// Support both the original constant-size sources and the new job setting.
func selectBlockSize(r *repo.Repository, kib int) int {
	if cfg, ok := any(r).(interface {
		SetVMBlockSizeKiB(int) error
		VMBlockSizeKiB() int
	}); ok {
		must(cfg.SetVMBlockSizeKiB(kib))
		return cfg.VMBlockSizeKiB() << 10
	}
	if kib != 0 && kib<<10 != diskimg.BlockSize {
		panic("block size does not match legacy binary")
	}
	return diskimg.BlockSize
}

// mixed fills each 4 KiB page with either random bytes (25% of pages), or
// a random 1 KiB motif repeated four times (75%). It is not a Windows image.
func mixed(b []byte, seed int64) {
	r := rand.New(rand.NewSource(seed))
	for off := 0; off < len(b); off += 4096 {
		p := b[off : off+4096]
		if off/4096%4 == 0 {
			_, _ = r.Read(p)
		} else {
			_, _ = r.Read(p[:1024])
			for j := 1024; j < 4096; j += 1024 {
				copy(p[j:j+1024], p[:1024])
			}
		}
	}
}

func fixtures(scenario string) ([][]byte, [][]byte, [][]diskimg.Area, []string, []string) {
	common := make([]byte, 80*MiB)
	mixed(common, 100)
	full, inc := make([][]byte, 3), make([][]byte, 3)
	areas := make([][]diskimg.Area, 3)
	hf, hi := []string{}, []string{}
	for vm := 0; vm < 3; vm++ {
		b := make([]byte, 128*MiB)
		copy(b, common)
		mixed(b[80*MiB:112*MiB], int64(200+vm))
		switch scenario {
		case "aligned":
		case "scattered":
			if vm > 0 { // one VM-specific 4 KiB edit per shared MiB
				r := rand.New(rand.NewSource(int64(300 + vm)))
				for m := 0; m < 80; m++ {
					off := m*MiB + ((m*7+vm*53)%256)*4096
					_, _ = r.Read(b[off : off+4096])
				}
			}
		case "shifted": // same common content, rotated by 4 or 8 KiB
			shift := vm * 4096
			copy(b[:80*MiB-shift], common[shift:])
			copy(b[80*MiB-shift:80*MiB], common[:shift])
		case "unique":
			_, _ = rand.New(rand.NewSource(int64(400 + vm))).Read(b[:112*MiB])
		default:
			panic("unknown scenario")
		}
		full[vm] = b
		c := append([]byte(nil), b...)
		r := rand.New(rand.NewSource(int64(500 + vm)))
		for m := 0; m < 112; m += 4 { // 112 KiB actually changed per VM
			off := m*MiB + ((m*11+vm*29)%256)*4096
			_, _ = r.Read(c[off : off+4096])
			// Deliberately coarse synthetic RCT: one complete MiB per edit.
			// This avoids assuming that Hyper-V reports 4 KiB regions.
			areas[vm] = append(areas[vm], diskimg.Area{Offset: uint64(m * MiB), Length: MiB})
		}
		inc[vm] = c
		a, z := sha256.Sum256(b), sha256.Sum256(c)
		hf = append(hf, hex.EncodeToString(a[:]))
		hi = append(hi, hex.EncodeToString(z[:]))
	}
	return full, inc, areas, hf, hi
}

func inventory(dir string) map[string]int64 {
	m := map[string]int64{"total": 0, "index": 0, "snapshots": 0, "data": 0, "files": 0}
	must(filepath.WalkDir(dir, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		i, e := d.Info()
		if e != nil {
			return e
		}
		m["total"] += i.Size()
		m["files"]++
		rel, _ := filepath.Rel(dir, p)
		for _, k := range []string{"index", "snapshots", "data"} {
			if len(rel) > len(k) && rel[:len(k)+1] == k+string(os.PathSeparator) {
				m[k] += i.Size()
			}
		}
		return nil
	}))
	return m
}

func backup(r *repo.Repository, disks [][]byte, areas [][]diskimg.Area, prev *repo.Snapshot, phase string) (*repo.Snapshot, map[string]any) {
	runtime.GC()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	before := r.Stats()
	var read uint64
	t := time.Now()
	sn := &repo.Snapshot{Time: time.Now().UTC(), Hostname: "synthetic-hyperv", Tags: []string{phase}, ProgramVersion: "0.47.3-blocksize-benchmark"}
	for vm, b := range disks {
		var base *repo.DiskImage
		ar := []diskimg.Area{{Offset: 0, Length: uint64(len(b))}}
		if prev != nil {
			base = &prev.Images[vm]
			ar = areas[vm]
		}
		img, e := diskimg.StoreAreas(ctx, r, bytes.NewReader(b), uint64(len(b)), ar, base, vm, fmt.Sprintf("vm%d", vm), "synthetic", func(n uint64) { read += n })
		must(e)
		sn.Images = append(sn.Images, img)
		sn.Guests = append(sn.Guests, repo.Guest{Platform: "hyperv", Type: "vm", ID: fmt.Sprint(vm), Name: fmt.Sprintf("vm%d", vm), Disks: []repo.GuestDisk{{Key: "disk0", Image: vm, Size: uint64(len(b))}}})
	}
	var e error
	sn.Tree, e = r.SaveTree(ctx, &repo.Tree{})
	must(e)
	must(r.Flush(ctx))
	_, e = r.SaveSnapshot(ctx, sn)
	must(e)
	elapsed := time.Since(t).Seconds()
	runtime.ReadMemStats(&m1)
	after := r.Stats()
	return sn, map[string]any{"seconds": elapsed, "read_bytes": read, "allocated_bytes": m1.TotalAlloc - m0.TotalAlloc, "new_blobs": after.NewBlobs - before.NewBlobs, "duplicate_calls": after.DupBlobs - before.DupBlobs, "new_raw_blob_bytes": after.RawBytes - before.RawBytes, "stored_blob_bytes": after.StoredBytes - before.StoredBytes, "indexed_blobs": r.Index().Len()}
}

func dataUnique(r *repo.Repository, sn *repo.Snapshot, seen map[repo.ID]bool) uint64 {
	var n uint64
	add := func(id repo.ID) {
		if id.IsNull() || seen[id] {
			return
		}
		seen[id] = true
		l, ok := r.BlobLength(id)
		if !ok {
			panic("missing data")
		}
		n += uint64(l)
	}
	for _, im := range sn.Images {
		add(im.Head)
		for _, p := range im.Partitions {
			ids, e := r.LoadBlockMap(ctx, &p)
			must(e)
			for _, id := range ids {
				add(id)
			}
		}
	}
	return n
}

type compareWriter struct {
	expected []byte
	calls    int
}

func (w *compareWriter) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 || off+int64(len(p)) > int64(len(w.expected)) || !bytes.Equal(p, w.expected[off:off+int64(len(p))]) {
		return 0, fmt.Errorf("restore mismatch at %d", off)
	}
	w.calls++
	return len(p), nil
}
func verify(r *repo.Repository, sn *repo.Snapshot, disks [][]byte) {
	for vm, b := range disks {
		w := &compareWriter{expected: b}
		must(diskimg.Write(ctx, r, &sn.Images[vm], w, true, nil))
		if w.calls == 0 {
			panic("empty restore")
		}
	}
}

func main() {
	scenario := flag.String("scenario", "scattered", "")
	dir := flag.String("repo", "", "")
	blockKiB := flag.Int("block-kib", 0, "")
	flag.Parse()
	if *dir == "" {
		panic("repo required")
	}
	if _, e := os.Stat(*dir); !os.IsNotExist(e) {
		panic("repo must not exist")
	}
	full, inc, areas, hf, hi := fixtures(*scenario)
	be, e := backend.OpenLocal(*dir)
	must(e)
	r, e := repo.Init(ctx, be, repo.Password(password))
	must(e)
	blockBytes := selectBlockSize(r, *blockKiB)
	sn, fm := backup(r, full, nil, nil, "full")
	fm["files"] = inventory(*dir)
	seen := map[repo.ID]bool{}
	fm["unique_data_raw_bytes"] = dataUnique(r, sn, seen)
	fullID := sn.ID
	must(r.Close())
	be, e = backend.OpenLocal(*dir)
	must(e)
	t := time.Now()
	r, e = repo.Open(ctx, be, repo.Password(password))
	must(e)
	openSeconds := time.Since(t).Seconds()
	selectBlockSize(r, *blockKiB)
	sn, e = r.LoadSnapshot(ctx, fullID.String())
	must(e)
	in, im := backup(r, inc, areas, sn, "incremental")
	im["files"] = inventory(*dir)
	im["unique_data_raw_bytes"] = dataUnique(r, in, seen)
	incID := in.ID
	must(r.Close())
	be, e = backend.OpenLocal(*dir)
	must(e)
	r, e = repo.Open(ctx, be, repo.Password(password))
	must(e)
	sn, e = r.LoadSnapshot(ctx, fullID.String())
	must(e)
	in, e = r.LoadSnapshot(ctx, incID.String())
	must(e)
	t = time.Now()
	verify(r, sn, full)
	verify(r, in, inc)
	verifySeconds := time.Since(t).Seconds()
	must(r.Close())
	out := map[string]any{"scenario": *scenario, "block_bytes": blockBytes, "vms": 3, "disk_bytes_per_vm": 128 * MiB, "changed_bytes_per_vm": 28 * 4096, "rct_range_bytes": MiB, "compression": "default", "encryption": "AES-256-GCM", "full": fm, "incremental": im, "reopen_seconds": openSeconds, "verified_disks": 6, "restore_compare_seconds": verifySeconds, "full_sha256": hf, "incremental_sha256": hi, "go_version": runtime.Version(), "gomaxprocs": runtime.GOMAXPROCS(0)}
	must(json.NewEncoder(os.Stdout).Encode(out))
}
