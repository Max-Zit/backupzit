package backend

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// Timed wraps a backend and counts how often, how long and how many bytes
// it saved and loaded, for throughput measurements (BACKUPZIT_PERF in the
// agent CLI). Busy times are summed over all calls: with several calls at
// once the sum exceeds the wall time, and sum/wall is the average number
// of calls in flight.
type Timed struct {
	Backend
	save, load, loadRange opStats
}

type opStats struct {
	n, bytes, nanos atomic.Int64
}

func (o *opStats) add(start time.Time, n int) {
	o.n.Add(1)
	o.bytes.Add(int64(n))
	o.nanos.Add(int64(time.Since(start)))
}

// NewTimed wraps be. It keeps be's upload concurrency.
func NewTimed(be Backend) *Timed { return &Timed{Backend: be} }

// Wrapped returns t as the backend to use: storage that makes objects
// immutable (S3 with Object Lock) keeps that ability.
func (t *Timed) Wrapped() Backend {
	if r, ok := t.Backend.(Retainer); ok {
		return &timedRetainer{t, r}
	}
	return t
}

type timedRetainer struct {
	*Timed
	Retainer
}

func (t *Timed) Concurrency() int { return Concurrency(t.Backend) }

func (t *Timed) Save(ctx context.Context, name string, data []byte) error {
	start := time.Now()
	err := t.Backend.Save(ctx, name, data)
	t.save.add(start, len(data))
	return err
}

func (t *Timed) Load(ctx context.Context, name string) ([]byte, error) {
	start := time.Now()
	b, err := t.Backend.Load(ctx, name)
	t.load.add(start, len(b))
	return b, err
}

func (t *Timed) LoadRange(ctx context.Context, name string, offset int64, length int) ([]byte, error) {
	start := time.Now()
	b, err := t.Backend.LoadRange(ctx, name, offset, length)
	t.loadRange.add(start, len(b))
	return b, err
}

// Report describes the counted calls; wall is the run's duration.
func (t *Timed) Report(wall time.Duration) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "perf: wall %.1fs", wall.Seconds())
	for _, op := range []struct {
		name string
		s    *opStats
	}{{"save", &t.save}, {"load", &t.load}, {"loadrange", &t.loadRange}} {
		n := op.s.n.Load()
		if n == 0 {
			continue
		}
		busy := time.Duration(op.s.nanos.Load())
		mb := float64(op.s.bytes.Load()) / 1e6
		fmt.Fprintf(&sb, "; %s %d calls, %.1f MB, busy %.1fs (%.1f in flight), %.2f ms/call, %.1f MB/s over wall",
			op.name, n, mb, busy.Seconds(), busy.Seconds()/max(wall.Seconds(), 1e-9),
			busy.Seconds()*1000/float64(n), mb/max(wall.Seconds(), 1e-9))
	}
	return sb.String()
}
