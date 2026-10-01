package testutil

import (
	"net/http/httptest"
	"strings"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

// S3Server is an in-memory S3 endpoint for tests.
type S3Server struct {
	srv *httptest.Server
	// Host is "127.0.0.1:port" for s3://Host/bucket?tls=false locations.
	Host string
}

// StartS3Server starts an in-memory S3 server with the given buckets.
func StartS3Server(buckets ...string) (*S3Server, error) {
	be := s3mem.New()
	for _, b := range buckets {
		if err := be.CreateBucket(b); err != nil {
			return nil, err
		}
	}
	srv := httptest.NewServer(gofakes3.New(be).Server())
	return &S3Server{srv: srv, Host: strings.TrimPrefix(srv.URL, "http://")}, nil
}

// Close stops the server.
func (s *S3Server) Close() { s.srv.Close() }
