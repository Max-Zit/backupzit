package backend

import (
	"context"
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
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Errorf("unlimited uploads took %s", d)
	}
	// Without a limiter in the context nothing waits.
	if err := be.Save(context.Background(), "z", data); err != nil {
		t.Fatal(err)
	}
}
