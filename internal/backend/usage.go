package backend

import (
	"context"
	"os"
	"path/filepath"
)

// Usage is the capacity of the file system or share that holds a
// repository, in bytes.
type Usage struct {
	Total, Free uint64
}

// Usager is implemented by storage that can tell its capacity (local and
// removable disks, SFTP servers with the statvfs extension, SMB shares).
// Object storage (S3, Azure) has no fixed capacity.
type Usager interface {
	Usage(ctx context.Context) (Usage, error)
}

// UsageOf returns the capacity of b's storage, if it can tell.
func UsageOf(ctx context.Context, b Backend) (Usage, bool) {
	for b != nil {
		if u, ok := b.(Usager); ok {
			x, err := u.Usage(ctx)
			return x, err == nil && x.Total > 0
		}
		w, ok := b.(interface{ Unwrap() Backend })
		if !ok {
			break
		}
		b = w.Unwrap()
	}
	return Usage{}, false
}

// Unwrap returns the throttled backend.
func (t *throttled) Unwrap() Backend { return t.Backend }

// Usage reports the capacity of the disk holding the directory (or its
// nearest existing parent: the repository may not exist yet).
func (l *Local) Usage(context.Context) (Usage, error) {
	dir := l.root
	for {
		if _, err := os.Stat(dir); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return diskUsage(dir)
}

// Usage asks the server with the statvfs@openssh.com extension.
func (s *SFTP) Usage(context.Context) (Usage, error) {
	st, err := s.client.StatVFS(s.root)
	if err != nil {
		return Usage{}, err
	}
	return Usage{Total: st.Blocks * st.Frsize, Free: st.Bavail * st.Frsize}, nil
}

// Usage reports the size of the share's volume.
func (s *SMB) Usage(context.Context) (Usage, error) {
	st, err := s.share.Statfs(s.root)
	if err != nil {
		return Usage{}, err
	}
	return Usage{Total: st.TotalBlockCount() * st.BlockSize(), Free: st.AvailableBlockCount() * st.BlockSize()}, nil
}
