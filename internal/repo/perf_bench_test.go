package repo

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
)

// shareLink models a file share on a LAN for benchmarks: every request
// costs round trips, and all transfers share one link of limited
// bandwidth (a transfer starts when the link is free). It has no
// Concurrency, so the repository treats it like SMB, SFTP or a local disk.
type shareLink struct {
	*memBackend
	rtt       time.Duration // one request/response, server time included
	bandwidth float64       // bytes per second
	saveTrips int           // round trips of a Save besides the data (mkdir, create, flush, close, rename)
	readTrips int           // round trips of a LoadRange besides the data (create, close)

	mu   sync.Mutex
	free time.Time // when the link has sent everything handed to it
}

func newShareLink() *shareLink {
	m := newMemBackend()
	m.discard = true
	return &shareLink{memBackend: m, rtt: 300 * time.Microsecond, bandwidth: 110e6, saveTrips: 5, readTrips: 2}
}

// transfer waits for trips round trips and n bytes on the shared link.
func (s *shareLink) transfer(ctx context.Context, trips, n int) error {
	s.mu.Lock()
	start := time.Now()
	if s.free.After(start) {
		start = s.free
	}
	s.free = start.Add(time.Duration(float64(n) / s.bandwidth * float64(time.Second)))
	until := s.free.Add(time.Duration(trips) * s.rtt)
	s.mu.Unlock()
	t := time.NewTimer(time.Until(until))
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *shareLink) Save(ctx context.Context, name string, data []byte) error {
	if strings.HasPrefix(name, "data/") {
		if err := s.transfer(ctx, s.saveTrips+1, len(data)); err != nil {
			return err
		}
	}
	return s.memBackend.Save(ctx, name, data)
}

func (s *shareLink) LoadRange(ctx context.Context, name string, off int64, n int) ([]byte, error) {
	if strings.HasPrefix(name, "data/") {
		if err := s.transfer(ctx, s.readTrips+1, n); err != nil {
			return nil, err
		}
	}
	return s.memBackend.LoadRange(ctx, name, off, n)
}

var _ backend.Backend = (*shareLink)(nil)

// BenchmarkBackupShare stores 128 blobs of 1 MiB (a VM disk run) on a
// simulated share, with SaveBlob (workers=1, file and image backup) and
// with the parallel BlobWriter (VM backup). The link alone would take
// about 1.2 s per GiB at 110 MB/s; the rest is time the pipeline waits.
func BenchmarkBackupShare(b *testing.B) {
	const n, size = 128, 1 << 20
	for _, profile := range []string{"compressible", "incompressible"} {
		blobs := testBlobs(profile, n, size, 3)
		for _, workers := range []int{1, 4} {
			b.Run(fmt.Sprintf("%s/workers=%d", profile, workers), func(b *testing.B) {
				ctx := context.Background()
				b.SetBytes(n * size)
				for b.Loop() {
					b.StopTimer()
					be := newShareLink()
					r := newTestRepo(b, be, DefaultPackSize)
					b.StartTimer()
					w, err := NewBlobWriter(ctx, r, workers)
					if err != nil {
						b.Fatal(err)
					}
					for _, d := range blobs {
						if _, _, err := w.Save(ctx, DataBlob, d); err != nil {
							b.Fatal(err)
						}
					}
					if err := w.Close(); err != nil {
						b.Fatal(err)
					}
					if err := r.Flush(ctx); err != nil {
						b.Fatal(err)
					}
					b.StopTimer()
					_ = r.Close()
					b.StartTimer()
				}
			})
		}
	}
}

// BenchmarkRestoreShare reads 128 blocks of 1 MiB in order through
// ReadAhead from a simulated share, as an image or VM restore does.
func BenchmarkRestoreShare(b *testing.B) {
	const n, size = 128, 1 << 20
	for _, profile := range []string{"compressible", "incompressible"} {
		b.Run(profile, func(b *testing.B) {
			ctx := context.Background()
			mem := newMemBackend()
			src := newTestRepo(b, mem, DefaultPackSize)
			ids := make([]ID, n)
			for i, d := range testBlobs(profile, n, size, 5) {
				id, _, err := src.SaveBlob(ctx, DataBlob, d)
				if err != nil {
					b.Fatal(err)
				}
				ids[i] = id
			}
			if err := src.Flush(ctx); err != nil {
				b.Fatal(err)
			}
			be := newShareLink()
			be.memBackend = mem
			r, err := Open(ctx, be)
			if err != nil {
				b.Fatal(err)
			}
			defer r.Close()
			b.SetBytes(n * size)
			for b.Loop() {
				ra := r.ReadAhead(ctx, ids, func(int) bool { return true })
				for i := range ids {
					if _, err := ra.Get(i); err != nil {
						b.Fatal(err)
					}
				}
				ra.Close()
			}
		})
	}
}
