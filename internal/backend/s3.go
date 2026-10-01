package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3 stores the repository in an S3 compatible object store (AWS S3,
// MinIO, Wasabi, Backblaze B2, Ceph, ...).
//
// Location: s3://endpoint[:port]/bucket[/prefix]. Add "?tls=false" for
// plain HTTP endpoints (e.g. a MinIO in the LAN without a certificate).
//
// Immutability: with Options.S3LockDays > 0 every object except lock files
// is written with an Object Lock retention in COMPLIANCE mode, so nobody
// (including the bucket owner and an attacker holding the keys) can delete
// or overwrite it before the retention ends. Deleting only adds a delete
// marker; the data version stays.
//
// Point-in-time view: with Options.AsOf set, the backend is read-only and
// shows every object as it was at that moment, using object versions. This
// recovers backups after an attacker deleted or overwrote objects.
type S3 struct {
	client   *minio.Client
	bucket   string
	prefix   string
	loc      string
	lockDays int

	asOf     time.Time
	versions map[string]s3Version // key -> version current at asOf
}

type s3Version struct {
	id   string
	size int64
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
	s := &S3{client: client, bucket: bucket, prefix: prefix, lockDays: opts.S3LockDays,
		loc: fmt.Sprintf("s3://%s/%s/%s", u.Host, bucket, prefix)}
	if s.lockDays > 0 {
		enabled, _, _, _, err := client.GetObjectLockConfig(ctx, bucket)
		if err != nil || enabled != "Enabled" {
			return nil, fmt.Errorf("s3: immutability requested but bucket %q has no Object Lock enabled (create it with Object Lock): %v", bucket, err)
		}
	}
	if !opts.AsOf.IsZero() {
		s.asOf = opts.AsOf
		if err := s.loadVersions(ctx); err != nil {
			return nil, err
		}
		s.loc += "@" + s.asOf.UTC().Format(time.RFC3339)
	}
	return s, nil
}

// loadVersions picks, for every key, the newest version not newer than asOf.
func (s *S3) loadVersions(ctx context.Context) error {
	type cand struct {
		v       s3Version
		t       time.Time
		deleted bool
	}
	best := map[string]cand{}
	listPrefix := ""
	if s.prefix != "" {
		listPrefix = s.prefix + "/"
	}
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: listPrefix, Recursive: true, WithVersions: true}) {
		if obj.Err != nil {
			return fmt.Errorf("s3 list versions: %w (is versioning / Object Lock enabled?)", obj.Err)
		}
		if obj.LastModified.After(s.asOf) {
			continue
		}
		c, ok := best[obj.Key]
		if !ok || obj.LastModified.After(c.t) {
			best[obj.Key] = cand{v: s3Version{id: obj.VersionID, size: obj.Size}, t: obj.LastModified, deleted: obj.IsDeleteMarker}
		}
	}
	s.versions = map[string]s3Version{}
	for k, c := range best {
		if !c.deleted {
			s.versions[k] = c.v
		}
	}
	return nil
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
	case "NoSuchKey", "NotFound", "NoSuchVersion":
		return fmt.Errorf("%s: %w", name, ErrNotFound)
	}
	return err
}

// getOpts returns options selecting the version for name in a
// point-in-time view.
func (s *S3) getOpts(name string) (minio.GetObjectOptions, error) {
	var o minio.GetObjectOptions
	if s.versions != nil {
		v, ok := s.versions[s.key(name)]
		if !ok {
			return o, fmt.Errorf("%s: %w", name, ErrNotFound)
		}
		o.VersionID = v.id
	}
	return o, nil
}

// Save uploads an object; S3 PUTs are atomic, so no temp object is needed.
func (s *S3) Save(ctx context.Context, name string, data []byte) error {
	if s.versions != nil {
		return ErrReadOnly
	}
	// Packs are ~16-24 MiB: a single PUT (up to 5 GiB) keeps uploads atomic
	// and avoids multipart bookkeeping.
	opts := minio.PutObjectOptions{ContentType: "application/octet-stream", DisableMultipart: true}
	if s.lockDays > 0 && !strings.HasPrefix(name, "locks/") {
		// One extra day keeps the object locked for the full period after
		// a long backup that reuses it (see KeepLocked).
		until := time.Now().Add(time.Duration(s.lockDays+1) * 24 * time.Hour).UTC()
		opts.Mode = minio.Compliance
		opts.RetainUntilDate = until
		opts.SendContentMd5 = true // required for PUTs with Object Lock
	}
	if _, err := s.client.PutObject(ctx, s.bucket, s.key(name), bytes.NewReader(data), int64(len(data)), opts); err != nil {
		return fmt.Errorf("s3 put %s: %w", name, err)
	}
	if s.lockDays > 0 && !opts.RetainUntilDate.IsZero() {
		retainCache.Store(s.cacheKey(name), opts.RetainUntilDate)
	}
	return nil
}

func (s *S3) Load(ctx context.Context, name string) ([]byte, error) {
	o, err := s.getOpts(name)
	if err != nil {
		return nil, err
	}
	obj, err := s.client.GetObject(ctx, s.bucket, s.key(name), o)
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
	o, err := s.getOpts(name)
	if err != nil {
		return nil, err
	}
	obj, err := s.client.GetObject(ctx, s.bucket, s.key(name), o)
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
	if s.versions != nil {
		v, ok := s.versions[s.key(name)]
		if !ok {
			return 0, fmt.Errorf("%s: %w", name, ErrNotFound)
		}
		return v.size, nil
	}
	st, err := s.client.StatObject(ctx, s.bucket, s.key(name), minio.StatObjectOptions{})
	if err != nil {
		return 0, s3NotFound(name, err)
	}
	return st.Size, nil
}

func (s *S3) List(ctx context.Context, dir string) ([]string, error) {
	prefix := s.key(dir) + "/"
	trim := func(k string) string {
		if s.prefix != "" {
			return strings.TrimPrefix(k, s.prefix+"/")
		}
		return k
	}
	var out []string
	if s.versions != nil {
		for k := range s.versions {
			if strings.HasPrefix(k, prefix) {
				out = append(out, trim(k))
			}
		}
		sort.Strings(out)
		return out, nil
	}
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("s3 list %s: %w", dir, obj.Err)
		}
		out = append(out, trim(obj.Key))
	}
	return out, nil
}

// Remove deletes an object. On buckets with Object Lock this only adds a
// delete marker; locked data versions remain until their retention ends
// (a bucket lifecycle rule should expire noncurrent versions afterwards).
func (s *S3) Remove(ctx context.Context, name string) error {
	if s.versions != nil {
		return ErrReadOnly
	}
	// S3 deletes are idempotent; report missing objects like other backends.
	if _, err := s.Size(ctx, name); err != nil {
		return err
	}
	return s.client.RemoveObject(ctx, s.bucket, s.key(name), minio.RemoveObjectOptions{})
}

func (s *S3) Close() error { return nil }

// retainCache remembers retain-until dates known to this process (keyed by
// bucket/key), so repeated backups do not query every shared pack again.
var retainCache sync.Map

func (s *S3) cacheKey(name string) string { return s.bucket + "/" + s.key(name) }

func (s *S3) LockDays() int { return s.lockDays }

// KeepLocked extends the COMPLIANCE retention of the named objects that
// would be unlocked within LockDays. Extended locks get 50% extra time, so
// data shared by daily backups is not touched on every run. Retention can
// only be extended, never shortened.
func (s *S3) KeepLocked(ctx context.Context, names []string) (int, error) {
	if s.lockDays <= 0 {
		return 0, nil
	}
	if s.versions != nil {
		return 0, ErrReadOnly
	}
	period := time.Duration(s.lockDays) * 24 * time.Hour
	need := time.Now().Add(period)
	extended := 0
	for _, name := range names {
		if strings.HasPrefix(name, "locks/") {
			continue
		}
		ck := s.cacheKey(name)
		if v, ok := retainCache.Load(ck); ok && !v.(time.Time).Before(need) {
			continue
		}
		_, until, err := s.client.GetObjectRetention(ctx, s.bucket, s.key(name), "")
		if err != nil && minio.ToErrorResponse(err).Code != "NoSuchObjectLockConfiguration" {
			return extended, fmt.Errorf("s3 retention of %s: %w", name, s3NotFound(name, err))
		}
		if until != nil && !until.Before(need) {
			retainCache.Store(ck, *until)
			continue
		}
		newUntil := need.Add(period / 2).UTC().Truncate(time.Second)
		mode := minio.Compliance
		if err := s.client.PutObjectRetention(ctx, s.bucket, s.key(name), minio.PutObjectRetentionOptions{
			Mode: &mode, RetainUntilDate: &newUntil}); err != nil {
			return extended, fmt.Errorf("s3 extend retention of %s: %w", name, err)
		}
		retainCache.Store(ck, newUntil)
		extended++
	}
	return extended, nil
}
