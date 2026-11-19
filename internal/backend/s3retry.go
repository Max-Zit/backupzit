package backend

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/minio/minio-go/v7"
)

// Object storage answers slowly or with an error now and then (Hetzner, for
// one, asks clients to retry). minio-go retries a request a few times by
// itself, but each attempt waits at most a minute for the response, so a
// provider that is slow for a few minutes still failed a whole VM (issue
// #38). The S3 backend therefore waits longer for responses and retries
// transient errors itself, with growing pauses. Retrying is safe: packs and
// other files are content-addressed or written whole with one PUT, and with
// Object Lock a repeat only adds a version of the same content.

// s3HeaderTimeout is how long a request waits for the response headers
// after it was sent (minio-go: one minute).
const s3HeaderTimeout = 5 * time.Minute

// s3ClientRetries are minio-go's own attempts per request; ours come on top.
const s3ClientRetries = 3

// s3Backoff are the pauses before our retries of a request (a variable for
// tests).
var s3Backoff = []time.Duration{5 * time.Second, 20 * time.Second, time.Minute}

// s3Transport is minio-go's default transport with the longer header
// timeout.
func s3Transport(secure bool) (http.RoundTripper, error) {
	tr, err := minio.DefaultTransport(secure)
	if err != nil {
		return nil, err
	}
	tr.ResponseHeaderTimeout = s3HeaderTimeout
	return tr, nil
}

// errShortRead marks a ranged read that returned fewer bytes than asked.
var errShortRead = errors.New("the storage returned fewer bytes than requested")

// transientS3 reports whether err is worth another attempt: timeouts,
// dropped connections, short reads, server errors and throttling. Missing
// objects, refused access and the caller's own cancellation are not.
func transientS3(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrClockSkew) {
		return false
	}
	if errors.Is(err, errShortRead) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	// Our own messages wrap the S3 error ("read data/…: %w"); minio's
	// ToErrorResponse only looks at the outermost error.
	var resp minio.ErrorResponse
	errors.As(err, &resp)
	switch resp.Code {
	case "SlowDown", "RequestTimeout", "InternalError", "ServiceUnavailable", "OperationAborted":
		return true
	}
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout {
		return true
	}
	msg := err.Error()
	for _, s := range []string{"timeout awaiting response headers", "connection reset", "broken pipe", "TLS handshake timeout", "unexpected EOF", "server closed idle connection", "did not respond in time"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// retryS3 runs f until it succeeds, fails for good or the retries are used
// up, pausing (with some jitter) between attempts.
func retryS3(ctx context.Context, f func() error) error {
	for i := 0; ; i++ {
		err := f()
		if err == nil || i == len(s3Backoff) || !transientS3(ctx, err) {
			return err
		}
		d := s3Backoff[i]
		d += time.Duration(rand.Int64N(int64(d)/5 + 1)) // #nosec G404 -- jitter, not security
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return err
		case <-t.C:
		}
	}
}
