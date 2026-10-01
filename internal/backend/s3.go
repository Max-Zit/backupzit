package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3 stores the repository in an S3 compatible object store (AWS S3,
// MinIO, Wasabi, Backblaze B2, Ceph, ...).
//
// Location: s3://endpoint[:port]/bucket[/prefix]. Add "?tls=false" for
// plain HTTP endpoints (e.g. a MinIO in the LAN without a certificate).
type S3 struct {
	client *minio.Client
	bucket string
	prefix string
	loc    string
}

// OpenS3 connects to the bucket named in u.
func OpenS3(ctx context.Context, u *url.URL, opts Options) (*S3, error) {
	if opts.S3AccessKey == "" || opts.S3SecretKey == "" {
		return nil, errors.New("s3: access key and secret key are required")
	}
	parts := strings.SplitN(strings.Trim(u.Path, "/"), "/", 2)
	if parts[0] == "" {
		return nil, errors.New("s3: bucket missing in location (s3://endpoint/bucket/prefix)")
	}
	bucket, prefix := parts[0], ""
	if len(parts) == 2 {
		prefix = strings.Trim(parts[1], "/")
	}
	secure := u.Query().Get("tls") != "false"
	client, err := minio.New(u.Host, &minio.Options{
		Creds:  credentials.NewStaticV4(opts.S3AccessKey, opts.S3SecretKey, ""),
		Secure: secure,
		Region: opts.S3Region,
	})
	if err != nil {
		return nil, fmt.Errorf("s3: %w", err)
	}
	ok, err := client.BucketExists(ctx, bucket)
	if err != nil {
		return nil, fmt.Errorf("s3: access bucket %q at %s: %w", bucket, u.Host, err)
	}
	if !ok {
		return nil, fmt.Errorf("s3: bucket %q does not exist at %s", bucket, u.Host)
	}
	return &S3{client: client, bucket: bucket, prefix: prefix, loc: fmt.Sprintf("s3://%s/%s/%s", u.Host, bucket, prefix)}, nil
}

func (s *S3) key(name string) string {
	if s.prefix == "" {
		return name
	}
	return path.Join(s.prefix, name)
}

func (s *S3) Location() string { return s.loc }

func s3NotFound(name string, err error) error {
	if err == nil {
		return nil
	}
	switch minio.ToErrorResponse(err).Code {
	case "NoSuchKey", "NotFound":
		return fmt.Errorf("%s: %w", name, ErrNotFound)
	}
	return err
}

// Save uploads an object; S3 PUTs are atomic, so no temp object is needed.
func (s *S3) Save(ctx context.Context, name string, data []byte) error {
	_, err := s.client.PutObject(ctx, s.bucket, s.key(name), bytes.NewReader(data), int64(len(data)),
		// Packs are ~16-24 MiB: a single PUT (up to 5 GiB) keeps uploads atomic
		// and avoids multipart bookkeeping.
		minio.PutObjectOptions{ContentType: "application/octet-stream", DisableMultipart: true})
	if err != nil {
		return fmt.Errorf("s3 put %s: %w", name, err)
	}
	return nil
}

func (s *S3) Load(ctx context.Context, name string) ([]byte, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, s.key(name), minio.GetObjectOptions{})
	if err != nil {
		return nil, s3NotFound(name, err)
	}
	defer obj.Close()
	b, err := io.ReadAll(obj)
	if err != nil {
		return nil, s3NotFound(name, err)
	}
	return b, nil
}

func (s *S3) LoadRange(ctx context.Context, name string, offset int64, length int) ([]byte, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, s.key(name), minio.GetObjectOptions{})
	if err != nil {
		return nil, s3NotFound(name, err)
	}
	defer obj.Close()
	if offset < 0 {
		st, err := obj.Stat()
		if err != nil {
			return nil, s3NotFound(name, err)
		}
		offset += st.Size
	}
	// ReadAt issues a ranged GET for exactly the requested bytes.
	buf := make([]byte, length)
	n, err := obj.ReadAt(buf, offset)
	if n == length {
		return buf, nil
	}
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	return nil, fmt.Errorf("read %s@%d+%d: %w", name, offset, length, s3NotFound(name, err))
}

func (s *S3) Size(ctx context.Context, name string) (int64, error) {
	st, err := s.client.StatObject(ctx, s.bucket, s.key(name), minio.StatObjectOptions{})
	if err != nil {
		return 0, s3NotFound(name, err)
	}
	return st.Size, nil
}

func (s *S3) List(ctx context.Context, dir string) ([]string, error) {
	prefix := s.key(dir) + "/"
	var out []string
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("s3 list %s: %w", dir, obj.Err)
		}
		name := obj.Key
		if s.prefix != "" {
			name = strings.TrimPrefix(name, s.prefix+"/")
		}
		out = append(out, name)
	}
	return out, nil
}

func (s *S3) Remove(ctx context.Context, name string) error {
	// S3 deletes are idempotent; report missing objects like other backends.
	if _, err := s.Size(ctx, name); err != nil {
		return err
	}
	return s.client.RemoveObject(ctx, s.bucket, s.key(name), minio.RemoveObjectOptions{})
}

func (s *S3) Close() error { return nil }
