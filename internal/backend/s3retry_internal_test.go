package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/testutil"
	"github.com/minio/minio-go/v7"
)

// flakyS3 sits in front of a fake S3 server and fails requests the way a
// slow provider does (issue #38).
type flakyS3 struct {
	next      http.Handler
	putFails  atomic.Int32 // PUTs still to answer with 503
	shortGets atomic.Int32 // ranged GETs still to cut short
	puts      atomic.Int32
}

func (f *flakyS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/data/") {
		f.puts.Add(1)
		if f.putFails.Add(-1) >= 0 {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `<Error><Code>SlowDown</Code><Message>slow down</Message></Error>`)
			return
		}
	}
	if r.Method == http.MethodGet && r.Header.Get("Range") != "" && f.shortGets.Add(-1) >= 0 {
		rec := httptest.NewRecorder()
		f.next.ServeHTTP(rec, r)
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		b := rec.Body.Bytes()
		_, _ = w.Write(b[:len(b)/2]) // the connection "drops" halfway
		return
	}
	f.next.ServeHTTP(w, r)
}

func TestS3RetriesTransientErrors(t *testing.T) {
	old := s3Backoff
	s3Backoff = []time.Duration{10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond}
	defer func() { s3Backoff = old }()

	srv, err := testutil.StartS3Server("bkt")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	target, _ := url.Parse("http://" + srv.Host)
	flaky := &flakyS3{next: httputil.NewSingleHostReverseProxy(target)}
	proxy := httptest.NewServer(flaky)
	defer proxy.Close()
	ctx := context.Background()
	be, err := Open(ctx, "s3://"+strings.TrimPrefix(proxy.URL, "http://")+"/bkt/p?tls=false", Options{S3AccessKey: "k", S3SecretKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("backupzit"), 50000)

	// More failures than minio-go retries by itself: only our retry saves it.
	flaky.putFails.Store(s3ClientRetries + 1)
	if err := be.Save(ctx, "data/aa/pack", data); err != nil {
		t.Fatalf("save with transient errors: %v", err)
	}
	if n := flaky.puts.Load(); n < s3ClientRetries+2 {
		t.Errorf("only %d PUT attempts", n)
	}

	// A ranged read that comes back short is read again.
	flaky.shortGets.Store(2)
	got, err := be.LoadRange(ctx, "data/aa/pack", 1000, 5000)
	if err != nil || !bytes.Equal(got, data[1000:6000]) {
		t.Fatalf("short read not retried: %v", err)
	}

	// A read that stays short says the file may be damaged.
	flaky.shortGets.Store(100)
	_, err = be.LoadRange(ctx, "data/aa/pack", 1000, 5000)
	if err == nil || !errors.Is(err, errShortRead) || !strings.Contains(err.Error(), "data check") {
		t.Fatalf("persistent short read: %v", err)
	}
	flaky.shortGets.Store(0)

	// Errors that will not go away are not retried.
	if _, err := be.Load(ctx, "data/aa/missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing object: %v", err)
	}
}

func TestTransientS3(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		err  error
		want bool
	}{
		{errors.New(`Put "https://x/y": net/http: timeout awaiting response headers`), true},
		{fmt.Errorf("read: %w", io.ErrUnexpectedEOF), true},
		{errors.New("read tcp 1.2.3.4:443: connection reset by peer"), true},
		{minio.ErrorResponse{Code: "SlowDown", StatusCode: 503}, true},
		{minio.ErrorResponse{Code: "InternalError", StatusCode: 500}, true},
		{minio.ErrorResponse{StatusCode: 429}, true},
		{minio.ErrorResponse{Code: "AccessDenied", StatusCode: 403}, false},
		{minio.ErrorResponse{Code: "NoSuchBucket", StatusCode: 404}, false},
		{fmt.Errorf("x: %w", ErrNotFound), false},
		{ErrClockSkew, false},
		{errors.New("s3: bucket does not exist"), false},
	} {
		if got := transientS3(ctx, c.err); got != c.want {
			t.Errorf("transientS3(%v) = %v, want %v", c.err, got, c.want)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if transientS3(cancelled, errors.New("timeout awaiting response headers")) {
		t.Error("retrying after the caller cancelled")
	}
}
