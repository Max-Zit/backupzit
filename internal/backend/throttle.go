package backend

import (
	"context"
	"sync"
	"time"
)

// Limiter paces uploads to a rate in bytes per second. The rate is asked
// for every upload, so it can change during a run (time windows).
type Limiter struct {
	rate func() int64
	mu   sync.Mutex
	next time.Time // when the next upload may start
}

// NewLimiter returns a limiter; rate returns 0 for no limit.
func NewLimiter(rate func() int64) *Limiter { return &Limiter{rate: rate} }

// Wait blocks until n more bytes may be sent.
func (l *Limiter) Wait(ctx context.Context, n int) error {
	r := l.rate()
	if r <= 0 {
		l.mu.Lock()
		l.next = time.Time{}
		l.mu.Unlock()
		return nil
	}
	l.mu.Lock()
	now := time.Now()
	start := l.next
	if start.Before(now) {
		start = now
	}
	l.next = start.Add(time.Duration(float64(n) / float64(r) * float64(time.Second)))
	l.mu.Unlock()
	// The first upload goes at once; the following ones keep the average
	// at the rate.
	d := time.Until(start)
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type limiterKey struct{}

// WithLimiter makes uploads of backends wrapped with Throttle under ctx
// wait for l.
func WithLimiter(ctx context.Context, l *Limiter) context.Context {
	return context.WithValue(ctx, limiterKey{}, l)
}

// Throttle wraps a backend so that Save honors the limiter in its context.
func Throttle(be Backend) Backend {
	t := &throttled{be}
	if r, ok := be.(Retainer); ok {
		return &throttledRetainer{t, r}
	}
	return t
}

type throttled struct{ Backend }

func (t *throttled) Save(ctx context.Context, name string, data []byte) error {
	if l, _ := ctx.Value(limiterKey{}).(*Limiter); l != nil {
		if err := l.Wait(ctx, len(data)); err != nil {
			return err
		}
	}
	return t.Backend.Save(ctx, name, data)
}

type throttledRetainer struct {
	*throttled
	Retainer
}
