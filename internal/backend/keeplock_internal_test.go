package backend

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
)

// A provider's timeout reaches the retry check wrapped in our own message
// ("read data/…: %w"); it must still count as temporary (issue #38:
// "The server did not respond in time." on a read during lock extension).
func TestTransientS3SeesWrappedErrors(t *testing.T) {
	ctx := context.Background()
	timeout := minio.ErrorResponse{Code: "RequestTimeout", StatusCode: 400, Message: "The server did not respond in time."}
	unavailable := minio.ErrorResponse{Code: "Busy", StatusCode: 503, Message: "busy"}
	denied := minio.ErrorResponse{Code: "AccessDenied", StatusCode: 403, Message: "denied"}
	for _, c := range []struct {
		err  error
		want bool
	}{
		{fmt.Errorf("read data/3b/x@7660210+36378: %w", timeout), true},
		{fmt.Errorf("block map 17c3: %w", fmt.Errorf("read: %w", unavailable)), true},
		{fmt.Errorf("read data/x: %w", denied), false},
		{errors.New("The server did not respond in time."), true},
	} {
		if got := transientS3(ctx, c.err); got != c.want {
			t.Errorf("%v: transient %v, want %v", c.err, got, c.want)
		}
	}
}

func fastRetries(t *testing.T) {
	old := s3Backoff
	s3Backoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { s3Backoff = old })
}

// One object that keeps failing does not stop the others from getting their
// lock extended; a temporary failure is retried; the error names what
// failed (issue #38: one slow answer stopped the whole step every night).
func TestExtendLocksContinuesAfterFailures(t *testing.T) {
	fastRetries(t)
	ctx := context.Background()
	prefix := fmt.Sprintf("test-%d/", time.Now().UnixNano())
	var names []string
	for i := 0; i < 10; i++ {
		names = append(names, fmt.Sprintf("data/%02d", i))
	}
	var flaky atomic.Int32
	extendedNames := map[string]bool{}
	get := func(_ context.Context, name string) (*time.Time, error) { return nil, nil }
	put := func(_ context.Context, name string, until time.Time) error {
		switch name {
		case "data/02": // fails once, then works
			if flaky.Add(1) == 1 {
				return minio.ErrorResponse{Code: "RequestTimeout", StatusCode: 400, Message: "The server did not respond in time."}
			}
		case "data/05": // keeps failing
			return minio.ErrorResponse{Code: "RequestTimeout", StatusCode: 400, Message: "The server did not respond in time."}
		}
		extendedNames[name] = true
		return nil
	}
	n, err := extendLocks(ctx, names, 30*24*time.Hour, func(s string) string { return prefix + s }, get, put)
	if n != 9 || len(extendedNames) != 9 || extendedNames["data/05"] {
		t.Errorf("extended %d (%v), want the 9 others", n, extendedNames)
	}
	if err == nil || !strings.Contains(err.Error(), "1 of 10 objects") || !strings.Contains(err.Error(), "data/05") {
		t.Errorf("error %v, want it to name data/05 and 1 of 10", err)
	}
	if flaky.Load() != 2 {
		t.Errorf("data/02 tried %d times, want a retry", flaky.Load())
	}
	// The next run only touches what is still missing.
	extendedNames = map[string]bool{}
	n, _ = extendLocks(ctx, names, 30*24*time.Hour, func(s string) string { return prefix + s }, get, put)
	if n != 0 || len(extendedNames) != 0 {
		t.Errorf("second run extended %v again", extendedNames)
	}
}

// A storage that keeps failing stops the step instead of waiting through
// the retries of every object.
func TestExtendLocksGivesUpWhenStorageIsDown(t *testing.T) {
	fastRetries(t)
	prefix := fmt.Sprintf("down-%d/", time.Now().UnixNano())
	names := make([]string, 50)
	for i := range names {
		names[i] = fmt.Sprintf("data/%02d", i)
	}
	var calls atomic.Int32
	get := func(_ context.Context, name string) (*time.Time, error) {
		calls.Add(1)
		return nil, minio.ErrorResponse{Code: "ServiceUnavailable", StatusCode: 503}
	}
	_, err := extendLocks(context.Background(), names, 24*time.Hour, func(s string) string { return prefix + s }, get, nil)
	if err == nil || !strings.Contains(err.Error(), "in a row") {
		t.Fatalf("error %v, want to stop", err)
	}
	// keepWorkers objects are asked at a time: at most those in flight when
	// the storage was given up, never all 50.
	per := int32(len(s3Backoff) + 1)
	if max := int32(keepGiveUp+keepWorkers) * per; calls.Load() < int32(keepGiveUp)*per || calls.Load() > max {
		t.Errorf("%d requests, want %d to %d", calls.Load(), int32(keepGiveUp)*per, max)
	}
	if !errors.Is(err, ErrKeepStopped) {
		t.Errorf("error %v does not mark the stop", err)
	}
}

// A lock that already lasts long enough is left alone.
func TestExtendLocksKeepsLongLocks(t *testing.T) {
	prefix := fmt.Sprintf("long-%d/", time.Now().UnixNano())
	later := time.Now().Add(60 * 24 * time.Hour)
	get := func(context.Context, string) (*time.Time, error) { return &later, nil }
	put := func(context.Context, string, time.Time) error { t.Error("extended a long lock"); return nil }
	if n, err := extendLocks(context.Background(), []string{"data/a", "locks/x"}, 30*24*time.Hour, func(s string) string { return prefix + s }, get, put); n != 0 || err != nil {
		t.Errorf("%d, %v", n, err)
	}
}

// R-2 (0.55): known retain-until dates survive a restart of the agent, so
// the next run does not ask about every object again (#38).
func TestRetainCacheSurvivesRestart(t *testing.T) {
	fastRetries(t)
	old := RetainCacheFile
	RetainCacheFile = filepath.Join(t.TempDir(), "retain-cache.json")
	defer func() { RetainCacheFile = old }()
	retainLoadOnce = sync.Once{}
	prefix := fmt.Sprintf("restart-%d/", time.Now().UnixNano())
	names := []string{"data/a", "data/b", "index/c"}
	var gets atomic.Int32
	get := func(context.Context, string) (*time.Time, error) { gets.Add(1); return nil, nil }
	put := func(context.Context, string, time.Time) error { return nil }
	if n, err := extendLocks(context.Background(), names, 30*24*time.Hour, func(s string) string { return prefix + s }, get, put); n != 3 || err != nil {
		t.Fatalf("first run: %d %v", n, err)
	}
	// "Restart": the memory is empty, the file is read again.
	for _, n := range names {
		retainCache.Delete(prefix + n)
	}
	retainLoadOnce = sync.Once{}
	gets.Store(0)
	if n, err := extendLocks(context.Background(), names, 30*24*time.Hour, func(s string) string { return prefix + s }, get, put); n != 0 || err != nil || gets.Load() != 0 {
		t.Fatalf("after a restart: extended %d, asked %d times, %v", n, gets.Load(), err)
	}
}
