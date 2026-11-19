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
	tr, err := s3Transport(secure)
	if err != nil {
		return nil, fmt.Errorf("s3: %w", err)
	}
	client, err := minio.New(u.Host, &minio.Options{
		Creds:      credentials.NewStaticV4(opts.S3AccessKey, opts.S3SecretKey, ""),
		Secure:     secure,
		Region:     opts.S3Region,
		Transport:  tr,
		MaxRetries: s3ClientRetries,
	})
	if err != nil {
		return nil, fmt.Errorf("s3: %w", err)
	}
	ok, err := client.BucketExists(ctx, bucket)
	if err != nil {
		return nil, fmt.Errorf("s3: access bucket %q at %s: %w", bucket, u.Host, withClockHint(err))
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
	return withClockHint(err)
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
	if err := retryS3(ctx, func() error {
		_, err := s.client.PutObject(ctx, s.bucket, s.key(name), bytes.NewReader(data), int64(len(data)), opts)
		return err
	}); err != nil {
		return fmt.Errorf("s3 put %s: %w", name, withClockHint(err))
	}
	if s.lockDays > 0 && !opts.RetainUntilDate.IsZero() {
		retainCache.Store(s.cacheKey(name), opts.RetainUntilDate)
	}
	return nil
}

func (s *S3) Load(ctx context.Context, name string) ([]byte, error) {
	var b []byte
	err := retryS3(ctx, func() (err error) {
		b, err = s.loadOnce(ctx, name)
		return err
	})
	return b, err
}

func (s *S3) loadOnce(ctx context.Context, name string) ([]byte, error) {
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

// LoadRange reads length bytes at offset. A short read is retried: it is
// usually a dropped connection, but if it repeats, the pack may really be
// shorter on the storage (damaged), and the message says so.
func (s *S3) LoadRange(ctx context.Context, name string, offset int64, length int) ([]byte, error) {
	var b []byte
	err := retryS3(ctx, func() (err error) {
		b, err = s.loadRangeOnce(ctx, name, offset, length)
		return err
	})
	if errors.Is(err, errShortRead) {
		return nil, fmt.Errorf("%w; it was read again several times, so the file may be damaged on the storage: run a data check (Check data on the job page)", err)
	}
	return b, err
}

func (s *S3) loadRangeOnce(ctx context.Context, name string, offset int64, length int) ([]byte, error) {
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
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, fmt.Errorf("read %s@%d+%d: got %d bytes: %w", name, offset, length, n, errShortRead)
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
	var size int64
	err := retryS3(ctx, func() error {
		st, err := s.client.StatObject(ctx, s.bucket, s.key(name), minio.StatObjectOptions{})
		if err != nil {
			return s3NotFound(name, err)
		}
		size = st.Size
		return nil
	})
	return size, err
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
	err := retryS3(ctx, func() error {
		out = out[:0]
		for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
			if obj.Err != nil {
				return obj.Err
			}
			out = append(out, trim(obj.Key))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("s3 list %s: %w", dir, withClockHint(err))
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
	return retryS3(ctx, func() error {
		return s.client.RemoveObject(ctx, s.bucket, s.key(name), minio.RemoveObjectOptions{})
	})
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
	get := func(name string) (*time.Time, error) {
		_, until, err := s.client.GetObjectRetention(ctx, s.bucket, s.key(name), "")
		if err != nil && minio.ToErrorResponse(err).Code == "NoSuchObjectLockConfiguration" {
			return nil, nil
		}
		if err != nil {
			return nil, s3NotFound(name, err)
		}
		return until, nil
	}
	put := func(name string, until time.Time) error {
		mode := minio.Compliance
		return s.client.PutObjectRetention(ctx, s.bucket, s.key(name), minio.PutObjectRetentionOptions{
			Mode: &mode, RetainUntilDate: &until})
	}
	return extendLocks(ctx, names, period, s.cacheKey, get, put)
}

// extendLocks extends the lock of each object that would end within period
// to period and a half. Each object gets the retries of other requests; one
// that still fails keeps its current lock (the next run tries again) and
// the others are extended anyway. Only a storage that keeps failing
// (keepGiveUp objects in a row) stops the step.
func extendLocks(ctx context.Context, names []string, period time.Duration, cacheKey func(string) string,
	get func(string) (*time.Time, error), put func(string, time.Time) error) (int, error) {
	need := time.Now().Add(period)
	extended := 0
	var failed, inARow int
	var firstErr error
	note := func(err error) error {
		failed++
		inARow++
		if firstErr == nil {
			firstErr = err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if inARow >= keepGiveUp {
			return fmt.Errorf("%d objects in a row could not be extended, stopping: %w", inARow, firstErr)
		}
		return nil
	}
	for _, name := range names {
		if strings.HasPrefix(name, "locks/") {
			continue
		}
		ck := cacheKey(name)
		if v, ok := retainCache.Load(ck); ok && !v.(time.Time).Before(need) {
			continue
		}
		var until *time.Time
		err := retryS3(ctx, func() error {
			var err error
			until, err = get(name)
			return err
		})
		if err != nil {
			if stop := note(fmt.Errorf("s3 retention of %s: %w", name, err)); stop != nil {
				return extended, stop
			}
			continue
		}
		if until != nil && !until.Before(need) {
			retainCache.Store(ck, *until)
			inARow = 0
			continue
		}
		newUntil := need.Add(period / 2).UTC().Truncate(time.Second)
		if err := retryS3(ctx, func() error { return put(name, newUntil) }); err != nil {
			if stop := note(fmt.Errorf("s3 extend retention of %s: %w", name, err)); stop != nil {
				return extended, stop
			}
			continue
		}
		retainCache.Store(ck, newUntil)
		extended++
		inARow = 0
	}
	if failed > 0 {
		return extended, fmt.Errorf("the lock of %d of %d objects could not be extended (they keep their current lock; the next run tries again): %w", failed, len(names), firstErr)
	}
	return extended, nil
}

// keepGiveUp is how many objects in a row may fail (after their retries)
// before extending locks stops: the storage is then down, not slow.
const keepGiveUp = 5

// DefaultRetention returns the bucket's default Object Lock retention
// (mode "COMPLIANCE" or "GOVERNANCE", and days), "" when the bucket has
// none. With a default retention in compliance mode, objects are locked by
// the bucket itself, even when a client (e.g. a compromised agent) writes
// them without a retention of its own.
func (s *S3) DefaultRetention(ctx context.Context) (mode string, days int, err error) {
	enabled, m, validity, unit, err := s.client.GetObjectLockConfig(ctx, s.bucket)
	if err != nil {
		return "", 0, err
	}
	if enabled != "Enabled" || m == nil || validity == nil || unit == nil {
		return "", 0, nil
	}
	days = int(*validity)
	if *unit == minio.Years {
		days *= 365
	}
	return string(*m), days, nil
}

// DefaultRetentionOf returns the default Object Lock retention of b when it
// is (or wraps) an S3 bucket; ok is false for other storage.
func DefaultRetentionOf(ctx context.Context, b Backend) (mode string, days int, ok bool, err error) {
	for b != nil {
		if s, is := b.(*S3); is {
			mode, days, err = s.DefaultRetention(ctx)
			return mode, days, true, err
		}
		w, is := b.(interface{ Unwrap() Backend })
		if !is {
			break
		}
		b = w.Unwrap()
	}
	return "", 0, false, nil
}
