package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"runtime"
	"runtime/metrics"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
)

// memBackend keeps objects in memory. failPacks > 0 makes the n-th pack
// upload (1-based) and every later one fail; discard drops pack contents.
type memBackend struct {
	mu        sync.Mutex
	objs      map[string][]byte
	packs     int
	failPacks int
	discard   bool
}

var errUpload = errors.New("upload failed")

func newMemBackend() *memBackend { return &memBackend{objs: map[string][]byte{}} }

func (m *memBackend) Save(_ context.Context, name string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.HasPrefix(name, "data/") {
		m.packs++
		if m.failPacks > 0 && m.packs >= m.failPacks {
			return errUpload
		}
		if m.discard {
			return nil
		}
	}
	m.objs[name] = append([]byte(nil), data...)
	return nil
}

func (m *memBackend) Load(_ context.Context, name string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[name]
	if !ok {
		return nil, backend.ErrNotFound
	}
	return append([]byte(nil), b...), nil
}

func (m *memBackend) LoadRange(ctx context.Context, name string, off int64, n int) ([]byte, error) {
	b, err := m.Load(ctx, name)
	if err != nil {
		return nil, err
	}
	if off < 0 {
		off += int64(len(b))
	}
	if off < 0 || off+int64(n) > int64(len(b)) {
		return nil, errors.New("range out of bounds")
	}
	return b[off : off+int64(n)], nil
}

func (m *memBackend) Size(_ context.Context, name string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[name]
	if !ok {
		return 0, backend.ErrNotFound
	}
	return int64(len(b)), nil
}

func (m *memBackend) List(_ context.Context, dir string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for n := range m.objs {
		if strings.HasPrefix(n, dir+"/") {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (m *memBackend) Remove(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objs, name)
	return nil
}

func (m *memBackend) Location() string { return "memory" }
func (m *memBackend) Close() error     { return nil }

func (m *memBackend) objects(prefix string) map[string][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string][]byte{}
	for n, b := range m.objs {
		if strings.HasPrefix(n, prefix) {
			out[n] = b
		}
	}
	return out
}

// testBlobs returns deterministic blobs: compressible text-like data,
// incompressible random data, or a mix with duplicates and small blobs.
func testBlobs(profile string, n, size int, seed uint64) [][]byte {
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	words := []string{"backup", "zit", "virtual", "disk", "block", "sector", "pack", "index ", "\n", "0000", "snapshot "}
	out := make([][]byte, 0, n)
	for i := range n {
		b := make([]byte, size)
		switch {
		case profile == "incompressible" || (profile == "mixed" && i%3 == 0):
			for k := 0; k+8 <= len(b); k += 8 {
				v := rng.Uint64()
				for s := range 8 {
					b[k+s] = byte(v >> (8 * s))
				}
			}
		default:
			var buf bytes.Buffer
			for buf.Len() < size {
				buf.WriteString(words[rng.IntN(len(words))])
				fmt.Fprintf(&buf, "%d ", rng.IntN(1000))
			}
			copy(b, buf.Bytes())
		}
		if profile == "mixed" {
			switch i % 7 {
			case 5:
				b = append([]byte(nil), out[i/2]...) // duplicate of an earlier blob
			case 6:
				b = b[:1+rng.IntN(4096)] // small blob
			}
		}
		out = append(out, b)
	}
	return out
}

func newTestRepo(t testing.TB, be backend.Backend, packSize int, opts ...Option) *Repository {
	t.Helper()
	r, err := Init(context.Background(), be, opts...)
	if err != nil {
		t.Fatal(err)
	}
	r.cfg.PackSize = packSize
	return r
}

type saveResult struct {
	id    ID
	added bool
}

// saveAll stores blobs with workers (1 = plain SaveBlob) and flushes.
func saveAll(t testing.TB, r *Repository, workers int, blobs [][]byte) []saveResult {
	t.Helper()
	ctx := context.Background()
	res := make([]saveResult, len(blobs))
	if workers == 0 {
		for i, b := range blobs {
			id, added, err := r.SaveBlob(ctx, DataBlob, b)
			if err != nil {
				t.Fatal(err)
			}
			res[i] = saveResult{id, added}
		}
	} else {
		w, err := NewBlobWriter(ctx, r, workers)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 0, 1<<20)
		for i, b := range blobs {
			// Reuse one buffer, as diskimg does: the writer must copy.
			buf = append(buf[:0], b...)
			id, added, err := w.Save(ctx, DataBlob, buf)
			if err != nil {
				t.Fatal(err)
			}
			res[i] = saveResult{id, added}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	return res
}

func waitGoroutines(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			t.Fatalf("goroutine leak: %d > %d\n%s", runtime.NumGoroutine(), before, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestBlobWriterIdenticalToSaveBlob: for an unencrypted repository the
// parallel writer produces byte-identical packs and indexes, the same IDs,
// "added" flags and statistics as sequential SaveBlob calls.
func TestBlobWriterIdenticalToSaveBlob(t *testing.T) {
	for _, level := range []string{CompressionDefault, CompressionFast, CompressionOff} {
		for _, profile := range []string{"compressible", "incompressible", "mixed"} {
			blobs := testBlobs(profile, 60, 256<<10, 42)
			seqBE := newMemBackend()
			seq := newTestRepo(t, seqBE, 2<<20)
			if err := seq.SetCompression(level); err != nil {
				t.Fatal(err)
			}
			want := saveAll(t, seq, 0, blobs)
			for _, workers := range []int{1, 2, 3, 8} {
				name := fmt.Sprintf("%s/%s/workers=%d", level, profile, workers)
				t.Run(name, func(t *testing.T) {
					before := runtime.NumGoroutine()
					be := newMemBackend()
					r := newTestRepo(t, be, 2<<20)
					if err := r.SetCompression(level); err != nil {
						t.Fatal(err)
					}
					got := saveAll(t, r, workers, blobs)
					for i := range want {
						if got[i] != want[i] {
							t.Fatalf("blob %d: got %+v, want %+v", i, got[i], want[i])
						}
					}
					if r.Stats() != seq.Stats() {
						t.Fatalf("stats %+v, want %+v", r.Stats(), seq.Stats())
					}
					for _, prefix := range []string{"data/", "index/"} {
						a, b := seqBE.objects(prefix), be.objects(prefix)
						if len(a) != len(b) {
							t.Fatalf("%s: %d objects, want %d", prefix, len(b), len(a))
						}
						for n, d := range a {
							if !bytes.Equal(b[n], d) {
								t.Fatalf("%s differs", n)
							}
						}
					}
					if len(seqBE.objects("data/")) < 2 && profile != "compressible" {
						t.Fatal("test should span several packs")
					}
					for i, b := range blobs {
						d, err := r.LoadBlob(context.Background(), DataBlob, got[i].id)
						if err != nil || !bytes.Equal(d, b) {
							t.Fatalf("blob %d reads back wrong (%v)", i, err)
						}
					}
					waitGoroutines(t, before)
				})
			}
		}
	}
}

// TestBlobWriterEncrypted: nonces are random, so packs differ, but the
// same blobs are stored, in the same packs, and read back identically.
func TestBlobWriterEncrypted(t *testing.T) {
	blobs := testBlobs("mixed", 40, 128<<10, 7)
	seqBE, parBE := newMemBackend(), newMemBackend()
	seq := newTestRepo(t, seqBE, 1<<20, Password("pw"))
	par := newTestRepo(t, parBE, 1<<20, Password("pw"))
	want := saveAll(t, seq, 0, blobs)
	got := saveAll(t, par, 4, blobs)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("blob %d: got %+v, want %+v", i, got[i], want[i])
		}
		d, err := par.LoadBlob(context.Background(), DataBlob, got[i].id)
		if err != nil || !bytes.Equal(d, blobs[i]) {
			t.Fatalf("blob %d reads back wrong (%v)", i, err)
		}
	}
	if seq.Stats() != par.Stats() {
		t.Fatalf("stats %+v, want %+v", par.Stats(), seq.Stats())
	}
	if len(seqBE.objects("data/")) != len(parBE.objects("data/")) {
		t.Fatal("different pack count")
	}
}

// TestBlobWriterUploadError: the first pack upload error reaches Save or
// Close, no goroutine stays behind, and blobs that never reached a pack
// are no longer "pending", so a later save stores them.
func TestBlobWriterUploadError(t *testing.T) {
	ctx := context.Background()
	before := runtime.NumGoroutine()
	be := newMemBackend()
	be.failPacks = 2
	r := newTestRepo(t, be, 512<<10)
	blobs := testBlobs("incompressible", 50, 128<<10, 3)
	w, err := NewBlobWriter(ctx, r, 4)
	if err != nil {
		t.Fatal(err)
	}
	var saveErr error
	saved := 0
	for _, b := range blobs {
		if _, _, saveErr = w.Save(ctx, DataBlob, b); saveErr != nil {
			break
		}
		saved++
	}
	closeErr := w.Close()
	if !errors.Is(closeErr, errUpload) {
		t.Fatalf("Close: %v, want upload error", closeErr)
	}
	if saveErr != nil && !errors.Is(saveErr, errUpload) {
		t.Fatalf("Save: %v", saveErr)
	}
	if saved == len(blobs) {
		t.Log("all saves accepted before the error; Close reported it")
	}
	if err := w.Close(); !errors.Is(err, errUpload) {
		t.Fatalf("second Close: %v", err)
	}
	if _, _, err := w.Save(ctx, DataBlob, blobs[0]); err == nil {
		t.Fatal("Save after Close succeeded")
	}
	waitGoroutines(t, before)
	// Blobs after the failed pack must not be considered stored.
	last := blobs[len(blobs)-1]
	r.mu.Lock()
	_, pending := r.pending[BlobHandle{Type: DataBlob, ID: Hash(last)}]
	r.mu.Unlock()
	if pending {
		t.Fatal("unstored blob still pending")
	}
}

// TestBlobWriterCancel: cancelling the caller's or the writer's context
// stops it without leaking goroutines.
func TestBlobWriterCancel(t *testing.T) {
	for _, which := range []string{"writer", "caller"} {
		t.Run(which, func(t *testing.T) {
			before := runtime.NumGoroutine()
			r := newTestRepo(t, newMemBackend(), 1<<20)
			wctx, wcancel := context.WithCancel(context.Background())
			cctx, ccancel := context.WithCancel(context.Background())
			defer wcancel()
			defer ccancel()
			w, err := NewBlobWriter(wctx, r, 4)
			if err != nil {
				t.Fatal(err)
			}
			blobs := testBlobs("compressible", 200, 64<<10, 9)
			var got error
			for i, b := range blobs {
				if i == 20 {
					if which == "writer" {
						wcancel()
					} else {
						ccancel()
					}
				}
				if _, _, err := w.Save(cctx, DataBlob, b); err != nil {
					got = err
					break
				}
			}
			if !errors.Is(got, context.Canceled) {
				t.Fatalf("Save: %v, want context.Canceled", got)
			}
			err = w.Close()
			if which == "writer" && !errors.Is(err, context.Canceled) {
				t.Fatalf("Close: %v, want context.Canceled", err)
			}
			waitGoroutines(t, before)
		})
	}
}

// --- benchmark ---

var peakHeapSample = []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}

func heapObjects() uint64 {
	metrics.Read(peakHeapSample)
	return peakHeapSample[0].Value.Uint64()
}

// BenchmarkBlobWriter stores 64 blobs of 1 MiB (one VM disk run) into a
// fresh repository per iteration, sequentially (SaveBlob, workers=1) and
// with N workers. peak-heap-MB is the highest live heap above the level
// before the iteration, sampled every 200 µs.
func BenchmarkBlobWriter(b *testing.B) {
	const n, size = 64, 1 << 20
	workerSet := []int{1, 2, 4}
	if p := runtime.GOMAXPROCS(0); p > 4 {
		workerSet = append(workerSet, min(p, 8))
	}
	for _, profile := range []string{"compressible", "incompressible"} {
		blobs := testBlobs(profile, n, size, 1)
		for _, workers := range workerSet {
			b.Run(fmt.Sprintf("%s/workers=%d", profile, workers), func(b *testing.B) {
				ctx := context.Background()
				b.SetBytes(n * size)
				b.ReportAllocs()
				var peak atomic.Uint64
				for b.Loop() {
					b.StopTimer()
					be := newMemBackend()
					be.discard = true
					r := newTestRepo(b, be, DefaultPackSize)
					runtime.GC()
					base := heapObjects()
					stop := make(chan struct{})
					sampled := make(chan struct{})
					go func() {
						defer close(sampled)
						tk := time.NewTicker(200 * time.Microsecond)
						defer tk.Stop()
						for {
							if h := heapObjects(); h > base && h-base > peak.Load() {
								peak.Store(h - base)
							}
							select {
							case <-stop:
								return
							case <-tk.C:
							}
						}
					}()
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
					close(stop)
					<-sampled
					_ = r.Close()
					b.StartTimer()
				}
				b.ReportMetric(float64(peak.Load())/(1<<20), "peak-heap-MB")
			})
		}
	}
}
