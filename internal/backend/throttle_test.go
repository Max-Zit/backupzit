package backend

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestThrottle(t *testing.T) {
	ctx := context.Background()
	mem, err := OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	be := Throttle(mem)
	rate := int64(1 << 20)
	ctx = WithLimiter(ctx, NewLimiter(func() int64 { return rate }))
	data := make([]byte, 256<<10)
	start := time.Now()
	for i := 0; i < 5; i++ { // 1.25 MiB at 1 MiB/s: the 5th waits ~1 s
		if err := be.Save(ctx, "x"+string(rune('a'+i)), data); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d < 900*time.Millisecond || d > 3*time.Second {
		t.Errorf("5 uploads took %s, want about 1 s", d)
	}
	rate = 0
	start = time.Now()
	for i := 0; i < 5; i++ {
		be.Save(ctx, "y", data)
	}
	// Throttled, these would take about 1 s; the margin is for a busy disk
	// (a loaded Windows test machine needed 275 ms).
	if d := time.Since(start); d > 800*time.Millisecond {
		t.Errorf("unlimited uploads took %s", d)
	}
	// Without a limiter in the context nothing waits.
	if err := be.Save(context.Background(), "z", data); err != nil {
		t.Fatal(err)
	}
}

// loadCounter counts whole loads; LoadMax comes from Local.
type loadCounter struct {
	*Local
	loads int
}

func (c *loadCounter) Load(ctx context.Context, name string) ([]byte, error) {
	c.loads++
	return c.Local.Load(ctx, name)
}

func (c *loadCounter) LockDays() int                                     { return 1 }
func (c *loadCounter) KeepLocked(context.Context, []string) (int, error) { return 0, nil }

// The agent wraps every storage with Throttle: the size limits of LoadMax
// (B7) must reach the storage below, not load the whole file first (MED 5,
// 0.63: a planted huge pack).
func TestThrottleKeepsTheSizeLimit(t *testing.T) {
	ctx := context.Background()
	local, err := OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Save(ctx, "data/big", make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	c := &loadCounter{Local: local}
	be := Throttle(c) // a Retainer: the other wrapper type
	if _, ok := be.(maxLoader); !ok {
		t.Fatal("the throttled backend has no LoadMax")
	}
	if _, err := LoadMax(ctx, be, "data/big", 100); !errors.Is(err, ErrTooLarge) {
		t.Errorf("over the limit: %v", err)
	}
	if b, err := LoadMax(ctx, be, "data/big", 4096); err != nil || len(b) != 4096 {
		t.Errorf("at the limit: %d bytes, %v", len(b), err)
	}
	if c.loads != 0 {
		t.Errorf("%d whole loads through the wrapper, want none", c.loads)
	}
	if _, ok := Throttle(local).(maxLoader); !ok {
		t.Error("the throttled plain backend has no LoadMax")
	}
}
