package testutil

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

// S3Server is an in-memory S3 endpoint for tests.
type S3Server struct {
	srv *httptest.Server
	// Host is "127.0.0.1:port" for s3://Host/bucket?tls=false locations.
	Host string

	mu       sync.Mutex
	truncate map[string]int // key part -> PUTs still to store short
	puts     map[string]int
	be       *s3mem.Backend
}

// StartS3Server starts an in-memory S3 server with the given buckets.
func StartS3Server(buckets ...string) (*S3Server, error) {
	be := s3mem.New()
	for _, b := range buckets {
		if err := be.CreateBucket(b); err != nil {
			return nil, err
		}
	}
	s := &S3Server{truncate: map[string]int{}, puts: map[string]int{}, be: be}
	fake := gofakes3.New(be).Server()
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.URL.RawQuery == "" {
			s.mu.Lock()
			short := false
			for part, n := range s.truncate {
				if strings.Contains(r.URL.Path, part) {
					s.puts[part]++
					if n > 0 {
						s.truncate[part] = n - 1
						short = true
					}
				}
			}
			s.mu.Unlock()
			if short {
				// Answer success, then keep only half of the object, as a
				// storage that loses data after it accepted it.
				fake.ServeHTTP(w, r)
				bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
				if obj, err := be.GetObject(bucket, key, nil); err == nil {
					body, _ := io.ReadAll(obj.Contents)
					_ = obj.Contents.Close()
					body = body[:len(body)/2]
					_, _ = be.PutObject(bucket, key, obj.Metadata, bytes.NewReader(body), int64(len(body)), nil)
				}
				return
			}
		}
		fake.ServeHTTP(w, r)
	}))
	s.Host = strings.TrimPrefix(s.srv.URL, "http://")
	return s, nil
}

// TruncateNextPuts makes the next n uploads of keys containing part be
// stored with half their content while answering success.
func (s *S3Server) TruncateNextPuts(part string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.truncate[part] = n
}

// Puts reports how many uploads of keys containing part arrived since
// TruncateNextPuts.
func (s *S3Server) Puts(part string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.puts[part]
}

// Replace overwrites an object directly in the store (no S3 request), for
// tests that damage data on the storage.
func (s *S3Server) Replace(bucket, key string, data []byte) error {
	_, err := s.be.PutObject(bucket, key, map[string]string{}, bytes.NewReader(data), int64(len(data)), nil)
	return err
}

// Close stops the server.
func (s *S3Server) Close() { s.srv.Close() }
