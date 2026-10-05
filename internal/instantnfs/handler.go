package instantnfs

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"os"
	"strings"
	"sync"

	billy "github.com/go-git/go-billy/v5"
	nfs "github.com/willscott/go-nfs"

	"github.com/max-zit/backupzit/internal/backend"
)

// Handler exposes an FS. File handles are an HMAC of the path with a secret
// key, recorded in a file: they stay valid when the server restarts (ESXi
// keeps using them) and cannot be guessed by other machines.
type Handler struct {
	fs      *FS
	root    string
	key     []byte
	mu      sync.Mutex
	paths   map[string][]string
	journal *os.File
	// Allowed decides which client addresses may mount and talk to the
	// server.
	Allowed func(net.IP) bool
}

// NewHandler serves fs; keyFile and journalFile keep the handles.
func NewHandler(fs *FS, root, keyFile, journalFile string) (*Handler, error) {
	key, err := os.ReadFile(keyFile)
	if err != nil || len(key) != 32 {
		key = make([]byte, 32)
		rand.Read(key)
		if err := os.WriteFile(keyFile, key, 0o600); err != nil {
			return nil, err
		}
	}
	h := &Handler{fs: fs, root: root, key: key, paths: map[string][]string{}}
	if f, err := os.Open(journalFile); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			if k, p, ok := strings.Cut(sc.Text(), "\t"); ok {
				h.paths[k] = splitPath(p)
			}
		}
		f.Close()
	}
	if h.journal, err = os.OpenFile(journalFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err != nil {
		return nil, err
	}
	return h, nil
}

func splitPath(p string) []string {
	if p == "" {
		return []string{}
	}
	return strings.Split(p, "/")
}

// Mount admits allowed clients to the whole tree.
func (h *Handler) Mount(_ context.Context, c net.Conn, _ nfs.MountRequest) (nfs.MountStatus, billy.Filesystem, []nfs.AuthFlavor) {
	if !h.allowed(c) {
		return nfs.MountStatusErrAcces, nil, nil
	}
	return nfs.MountStatusOk, h.fs, []nfs.AuthFlavor{nfs.AuthFlavorNull}
}

func (h *Handler) allowed(c net.Conn) bool {
	if h.Allowed == nil {
		return false
	}
	a, ok := c.RemoteAddr().(*net.TCPAddr)
	return ok && h.Allowed(a.IP)
}

// Change sets attributes.
func (h *Handler) Change(billy.Filesystem) billy.Change { return h.fs }

// FSStat reports the space of the directory's disk.
func (h *Handler) FSStat(ctx context.Context, _ billy.Filesystem, s *nfs.FSStat) error {
	l, err := backend.OpenLocal(h.root)
	if err != nil {
		return nil
	}
	if u, ok := backend.UsageOf(ctx, l); ok {
		s.TotalSize, s.FreeSize, s.AvailableSize = u.Total, u.Free, u.Free
		s.TotalFiles, s.FreeFiles, s.AvailableFiles = 1<<24, 1<<23, 1<<23
	}
	return nil
}

// ToHandle returns the handle of a path.
func (h *Handler) ToHandle(_ billy.Filesystem, path []string) []byte {
	p := strings.Join(path, "/")
	m := hmac.New(sha256.New, h.key)
	m.Write([]byte(p))
	sum := m.Sum(nil)[:24]
	k := hex.EncodeToString(sum)
	h.mu.Lock()
	if _, ok := h.paths[k]; !ok {
		h.paths[k] = append([]string{}, path...)
		h.journal.WriteString(k + "\t" + p + "\n")
	}
	h.mu.Unlock()
	return sum
}

// FromHandle resolves a handle.
func (h *Handler) FromHandle(fh []byte) (billy.Filesystem, []string, error) {
	h.mu.Lock()
	p, ok := h.paths[hex.EncodeToString(fh)]
	h.mu.Unlock()
	if !ok {
		return nil, nil, &nfs.NFSStatusError{NFSStatus: nfs.NFSStatusStale}
	}
	return h.fs, append([]string{}, p...), nil
}

// InvalidateHandle keeps handles: a path that is gone is reported as such.
func (h *Handler) InvalidateHandle(billy.Filesystem, []byte) error { return nil }

// HandleLimit is not limited.
func (h *Handler) HandleLimit() int { return 1 << 30 }
