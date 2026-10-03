package vmware

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync"
)

// dsPath is a datastore path "[datastore] dir/file".
type dsPath struct{ DS, Path string }

func parseDSPath(s string) (dsPath, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[") {
		return dsPath{}, fmt.Errorf("not a datastore path: %q", s)
	}
	ds, p, ok := strings.Cut(s[1:], "]")
	if !ok {
		return dsPath{}, fmt.Errorf("not a datastore path: %q", s)
	}
	return dsPath{DS: ds, Path: strings.TrimSpace(p)}, nil
}

func (p dsPath) String() string { return "[" + p.DS + "] " + p.Path }

// Dir is the directory of the file.
func (p dsPath) Dir() string { return path.Dir(p.Path) }

// local is the path in the ESXi shell.
func (p dsPath) local() string { return "/vmfs/volumes/" + p.DS + "/" + p.Path }

func (p dsPath) join(name string) dsPath { return dsPath{DS: p.DS, Path: path.Join(p.Dir(), name)} }

// fileURL is the HTTPS address of a datastore file.
func (c *Client) fileURL(p dsPath) string {
	u := *c.vim.URL()
	u.Path = "/folder/" + strings.TrimPrefix(p.Path, "/")
	u.RawQuery = url.Values{"dcPath": {c.dc.Name()}, "dsName": {p.DS}}.Encode()
	return u.String()
}

// get downloads a (small) datastore file.
func (c *Client) get(ctx context.Context, p dsPath) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.fileURL(p), nil)
	if err != nil {
		return nil, err
	}
	var b []byte
	err = c.vim.Do(ctx, req, func(resp *http.Response) error {
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("download %s: %s", p, resp.Status)
		}
		var err error
		b, err = io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		return err
	})
	return b, err
}

// put uploads a (small) datastore file.
func (c *Client) put(ctx context.Context, p dsPath, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.fileURL(p), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(data))
	req.Header.Set("Content-Type", "application/octet-stream")
	return c.vim.Do(ctx, req, func(resp *http.Response) error {
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
			return fmt.Errorf("upload %s: %s", p, resp.Status)
		}
		return nil
	})
}

// fileReader reads a datastore file with HTTP range requests.
type fileReader struct {
	ctx context.Context
	c   *Client
	url string
}

func (r *fileReader) ReadAt(b []byte, off int64) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+int64(len(b))-1))
	var n int
	err = r.c.vim.Do(r.ctx, req, func(resp *http.Response) error {
		switch resp.StatusCode {
		case http.StatusPartialContent:
		case http.StatusRequestedRangeNotSatisfiable:
			return io.EOF
		default:
			return fmt.Errorf("read disk: %s", resp.Status)
		}
		var err error
		n, err = io.ReadFull(resp.Body, b)
		if errors.Is(err, io.ErrUnexpectedEOF) {
			err = io.EOF
		}
		return err
	})
	return n, err
}

// extentRe matches "RW 16777216 VMFS "name-flat.vmdk"".
var extentRe = regexp.MustCompile(`(?m)^(RW|RDONLY|NOACCESS)\s+(\d+)\s+(\w+)\s+"([^"]+)"`)

// flatFile reads a disk's descriptor and returns its data file, which must
// be a single flat extent (thin or thick VMFS disks).
func (c *Client) flatFile(ctx context.Context, disk dsPath) (dsPath, error) {
	desc, err := c.get(ctx, disk)
	if err != nil {
		return dsPath{}, err
	}
	if len(desc) > 0 && !bytes.Contains(desc[:min(len(desc), 4096)], []byte("Disk DescriptorFile")) {
		return dsPath{}, fmt.Errorf("%s is not a VMDK descriptor", disk)
	}
	ext := extentRe.FindAllSubmatch(desc, -1)
	if len(ext) != 1 || !strings.EqualFold(string(ext[0][3]), "VMFS") {
		ct := regexp.MustCompile(`createType="([^"]*)"`).FindSubmatch(desc)
		kind := "unknown"
		if ct != nil {
			kind = string(ct[1])
		}
		return dsPath{}, fmt.Errorf("%s is a %q disk; only flat VMFS disks can be read (consolidate or remove the VM's snapshots, RDMs are not supported)", disk, kind)
	}
	return disk.join(string(ext[0][4])), nil
}

// diskWriter writes blocks into a disk file on the host through a small
// helper running in the ESXi Python over SSH. Blocks never written stay
// unallocated in thin disks.
type diskWriter struct {
	mu    sync.Mutex
	w     *bufio.Writer
	stdin io.WriteCloser
	wait  func() error
	out   *bytes.Buffer
	err   error
}

const writerScript = `import os,sys,struct
fd=os.open(sys.argv[1],os.O_WRONLY)
r=sys.stdin.buffer
while True:
    h=r.read(12)
    if len(h)<12: sys.exit(2)
    off,n=struct.unpack('<QI',h)
    if n==0: break
    d=r.read(n)
    if len(d)!=n: sys.exit(3)
    os.pwrite(fd,d,off)
os.fsync(fd)
os.close(fd)
`

func (c *Client) openWriter(ctx context.Context, file dsPath) (*diskWriter, error) {
	cl, err := c.sshClient()
	if err != nil {
		return nil, err
	}
	s, err := cl.NewSession()
	if err != nil {
		return nil, err
	}
	stdin, err := s.StdinPipe()
	if err != nil {
		s.Close()
		return nil, err
	}
	out := &bytes.Buffer{}
	s.Stdout, s.Stderr = out, out
	if err := s.Start("python3 -c " + sq(writerScript) + " " + sq(file.local())); err != nil {
		s.Close()
		return nil, err
	}
	go func() {
		<-ctx.Done()
		s.Close()
	}()
	return &diskWriter{w: bufio.NewWriterSize(stdin, 4<<20), stdin: stdin, out: out,
		wait: func() error { defer s.Close(); return s.Wait() }}, nil
}

func (w *diskWriter) WriteAt(b []byte, off int64) (int, error) {
	total := len(b)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}
	for len(b) > 0 {
		n := min(len(b), 4<<20)
		var h [12]byte
		binary.LittleEndian.PutUint64(h[:8], uint64(off))
		binary.LittleEndian.PutUint32(h[8:], uint32(n))
		if _, err := w.w.Write(h[:]); err != nil {
			w.err = err
			return 0, err
		}
		if _, err := w.w.Write(b[:n]); err != nil {
			w.err = err
			return 0, err
		}
		b, off = b[n:], off+int64(n)
	}
	return total, nil
}

// Close finishes the stream and reports whether all data arrived.
func (w *diskWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	var h [12]byte
	binary.LittleEndian.PutUint64(h[:8], 0)
	w.w.Write(h[:])
	if err := w.w.Flush(); err != nil && w.err == nil {
		w.err = err
	}
	w.stdin.Close()
	err := w.wait()
	if w.err != nil {
		return w.err
	}
	// The helper exits with 0 only after the end marker; ESXi may drop its
	// output after a large upload, so the exit status is what counts.
	if err != nil {
		return fmt.Errorf("writing the disk on the host failed: %v %s", err, strings.TrimSpace(lastLines(w.out.String(), 3)))
	}
	return nil
}
