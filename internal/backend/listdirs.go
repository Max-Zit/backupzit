package backend

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/minio/minio-go/v7"
)

// DirLister is implemented by storage that can list the folders directly
// below a folder without walking everything inside them.
type DirLister interface {
	ListDirs(ctx context.Context, dir string) ([]string, error)
}

// RepositoryDirs lists the folders directly below the storage location that
// hold a repository (they have a "config" object), sorted by name.
func RepositoryDirs(ctx context.Context, be Backend) ([]string, error) {
	var dirs []string
	if dl, ok := be.(DirLister); ok {
		list, err := dl.ListDirs(ctx, "")
		if err != nil {
			return nil, err
		}
		for _, d := range list {
			if _, err := be.Size(ctx, d+"/config"); err == nil {
				dirs = append(dirs, d)
			} else if !errors.Is(err, ErrNotFound) {
				return nil, err
			}
		}
	} else {
		names, err := be.List(ctx, "")
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			if d, f, ok := strings.Cut(n, "/"); ok && f == "config" {
				dirs = append(dirs, d)
			}
		}
	}
	sort.Strings(dirs)
	return dirs, nil
}

func (s *SFTP) ListDirs(_ context.Context, dir string) ([]string, error) {
	entries, err := s.client.ReadDir(s.p(dir))
	if err != nil {
		return nil, wrapNotFound(dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

func (s *SMB) ListDirs(_ context.Context, dir string) (out []string, err error) {
	// See List: a malformed listing must not crash the process.
	defer func() {
		if p := recover(); p != nil {
			out, err = nil, fmt.Errorf("smb: invalid directory listing from server: %v", p)
		}
	}()
	entries, err := s.share.ReadDir(s.p(dir))
	if err != nil {
		return nil, smbNotFound(dir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

func (l *Local) ListDirs(_ context.Context, dir string) ([]string, error) {
	entries, err := os.ReadDir(l.path(dir))
	if err != nil {
		return nil, wrapNotFound(dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

func (s *S3) ListDirs(ctx context.Context, dir string) ([]string, error) {
	prefix := s.key(dir)
	if prefix != "" {
		prefix += "/"
	}
	seen := map[string]bool{}
	var out []string
	add := func(rest string) {
		if d, _, ok := strings.Cut(rest, "/"); ok && d != "" && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	if s.versions != nil {
		for k := range s.versions {
			if strings.HasPrefix(k, prefix) {
				add(k[len(prefix):])
			}
		}
		return out, nil
	}
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("s3 list %s: %w", dir, obj.Err)
		}
		add(obj.Key[len(prefix):])
	}
	return out, nil
}

func (a *Azure) ListDirs(ctx context.Context, dir string) ([]string, error) {
	prefix := a.key(dir)
	if prefix != "" {
		prefix += "/"
	}
	var out []string
	pager := a.c.NewListBlobsHierarchyPager("/", &container.ListBlobsHierarchyOptions{Prefix: &prefix})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("azure list %s: %w", dir, shortAzureErr(err))
		}
		for _, p := range page.Segment.BlobPrefixes {
			if p.Name != nil {
				if d := strings.TrimSuffix(strings.TrimPrefix(*p.Name, prefix), "/"); d != "" {
					out = append(out, d)
				}
			}
		}
	}
	return out, nil
}
