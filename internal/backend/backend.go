// Package backend abstracts the storage where a repository lives
// (local directory, SFTP, later S3 and SMB).
//
// All names are slash-separated paths relative to the repository root,
// e.g. "config", "snapshots/<id>", "data/ab/<id>".
package backend

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ErrNotFound is returned when a named object does not exist.
var ErrNotFound = errors.New("backend: object not found")

// Backend is a flat object store addressed by slash-separated names.
type Backend interface {
	// Save atomically stores data under name. Readers never observe a
	// partially written object.
	Save(ctx context.Context, name string, data []byte) error
	// Load returns the full content of name.
	Load(ctx context.Context, name string) ([]byte, error)
	// LoadRange returns length bytes starting at offset. A negative offset
	// counts from the end of the object.
	LoadRange(ctx context.Context, name string, offset int64, length int) ([]byte, error)
	// Size returns the size of name in bytes.
	Size(ctx context.Context, name string) (int64, error)
	// List returns all object names below dir (recursively), relative to
	// the repository root.
	List(ctx context.Context, dir string) ([]string, error)
	// Remove deletes name.
	Remove(ctx context.Context, name string) error
	// Location describes the backend for humans/logs.
	Location() string
	Close() error
}

// Options holds credentials and settings needed to open a backend.
type Options struct {
	SFTPPassword string
	SFTPKeyFile  string
	SFTPHostKey  string // pinned host key fingerprint, "SHA256:..."
	SFTPInsecure bool   // skip host key verification (tests only)
	S3AccessKey  string
	S3SecretKey  string
	S3Region     string
}

// Open parses a repository location and opens the matching backend.
//
//	/path/to/repo, C:\repo, local:/path   -> local directory
//	sftp://user@host[:port]/path          -> SFTP
//	s3://endpoint[:port]/bucket[/prefix]   -> S3 (?tls=false for plain HTTP)
func Open(ctx context.Context, location string, opts Options) (Backend, error) {
	switch {
	case strings.HasPrefix(location, "sftp://"):
		u, err := url.Parse(location)
		if err != nil {
			return nil, fmt.Errorf("parse sftp url: %w", err)
		}
		return OpenSFTP(ctx, u, opts)
	case strings.HasPrefix(location, "s3://"):
		u, err := url.Parse(location)
		if err != nil {
			return nil, fmt.Errorf("parse s3 url: %w", err)
		}
		return OpenS3(ctx, u, opts)
	case strings.HasPrefix(location, "local:"):
		return OpenLocal(strings.TrimPrefix(location, "local:"))
	case strings.Contains(location, "://"):
		return nil, fmt.Errorf("unsupported repository scheme in %q", location)
	default:
		return OpenLocal(location)
	}
}
