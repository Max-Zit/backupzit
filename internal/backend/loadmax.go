package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
)

// Reading files of the repository with a size limit per kind (B7, 0.62):
// anyone who can write to the storage can plant a huge file where a small
// one is expected (a lock, a snapshot, an index file), and loading it
// whole would exhaust the agent's or the console's memory.

// ErrTooLarge reports an object larger than its kind may be.
var ErrTooLarge = errors.New("backend: object too large")

// maxLoader is implemented by the backends that can stop reading at the
// limit in the same request.
type maxLoader interface {
	LoadMax(ctx context.Context, name string, max int64) ([]byte, error)
}

// LoadMax loads name but refuses objects larger than max bytes. The storage
// backends stop reading at the limit; others (wrappers) load the object
// and refuse it afterwards, without an extra request.
func LoadMax(ctx context.Context, be Backend, name string, max int64) ([]byte, error) {
	if m, ok := be.(maxLoader); ok {
		return m.LoadMax(ctx, name, max)
	}
	b, err := be.Load(ctx, name)
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, tooLarge(name, max)
	}
	return b, nil
}

func tooLarge(name string, max int64) error {
	return fmt.Errorf("%s is larger than %d bytes, more than this kind of file can be: %w", name, max, ErrTooLarge)
}

// readMax reads r to its end, at most max bytes.
func readMax(r io.Reader, name string, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, tooLarge(name, max)
	}
	return b, nil
}

// LoadMax implements maxLoader.
func (l *Local) LoadMax(ctx context.Context, name string, max int64) ([]byte, error) {
	f, err := l.open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > max {
		return nil, tooLarge(name, max)
	}
	return readMax(f, name, max)
}

// LoadMax implements maxLoader.
func (s *SFTP) LoadMax(_ context.Context, name string, max int64) ([]byte, error) {
	f, err := s.client.Open(s.p(name))
	if err != nil {
		return nil, wrapNotFound(name, err)
	}
	defer f.Close()
	return readMax(f, name, max)
}

// LoadMax implements maxLoader.
func (s *SMB) LoadMax(_ context.Context, name string, max int64) ([]byte, error) {
	f, err := s.share.Open(s.p(name))
	if err != nil {
		return nil, smbNotFound(name, err)
	}
	defer f.Close()
	return readMax(f, name, max)
}

// LoadMax implements maxLoader.
func (s *S3) LoadMax(ctx context.Context, name string, max int64) ([]byte, error) {
	var b []byte
	err := retryS3(ctx, func() error {
		o, err := s.getOpts(name)
		if err != nil {
			return err
		}
		obj, err := s.client.GetObject(ctx, s.bucket, s.key(name), o)
		if err != nil {
			return s3NotFound(name, err)
		}
		defer obj.Close()
		b, err = readMax(obj, name, max)
		if errors.Is(err, ErrTooLarge) {
			return err
		}
		if err != nil {
			return s3NotFound(name, err)
		}
		return nil
	})
	return b, err
}

// LoadMax implements maxLoader.
func (h *Hardened) LoadMax(ctx context.Context, name string, max int64) ([]byte, error) {
	resp, err := h.do(ctx, http.MethodGet, h.path("files", h.name(name)), nil, nil)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", name, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("load %s: %w", name, statusError(resp))
	}
	defer resp.Body.Close()
	if resp.ContentLength > max {
		return nil, tooLarge(name, max)
	}
	return readMax(resp.Body, name, max)
}

// LoadMax implements maxLoader.
func (a *Azure) LoadMax(ctx context.Context, name string, max int64) ([]byte, error) {
	resp, err := a.c.NewBlobClient(a.key(name)).DownloadStream(ctx, &blob.DownloadStreamOptions{})
	if err != nil {
		return nil, azNotFound(name, err)
	}
	defer resp.Body.Close()
	if resp.ContentLength != nil && *resp.ContentLength > max {
		return nil, tooLarge(name, max)
	}
	return readMax(resp.Body, name, max)
}
