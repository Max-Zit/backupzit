package backend

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"sync"
	"time"
)

// Reads from an SMB share (#71).
//
// A restore reads many blocks of one pack in a row; opening and closing the
// file for each cost two extra round trips per block. So the handles of
// recently read packs stay open, at most smbHandleCache of them. go-smb2
// opens files without delete sharing, though: while a handle is open, a
// prune of another agent or of the console could not delete that pack
// (sharing violation). A handle is therefore closed when it has not been
// used for smbHandleIdle, before the pack is saved or removed through this
// backend, after a failed read, and when the backend closes.

// smbFile is what a read needs of an open file (tests use their own).
type smbFile interface {
	io.ReaderAt
	io.Closer
	Stat() (fs.FileInfo, error)
}

const (
	smbHandleCache = 8
	smbHandleIdle  = 2 * time.Second
	// smbOneRead is the largest object read with one ReadAt sized from the
	// file (go-smb2 splits it into requests of the largest size the server
	// takes); io.ReadAll grew its buffer in small steps. A larger or
	// negative size, as a planted sparse file or an odd server may report,
	// is read as a stream instead of being allocated at once.
	smbOneRead = 512 << 20
)

type smbHandle struct {
	f       smbFile
	refs    int
	used    uint64    // order of use, for evicting the oldest
	idle    time.Time // when the last read ended
	evicted bool      // out of the cache: closed when its last read ends
}

// smbHandles is the cache of open pack handles of one SMB backend.
type smbHandles struct {
	mu       sync.Mutex
	open     map[string]*smbHandle
	seq      uint64
	sweeping bool
	idleFor  time.Duration // 0: smbHandleIdle (tests shorten it)
	closed   bool
}

func (c *smbHandles) idleTime() time.Duration {
	if c.idleFor > 0 {
		return c.idleFor
	}
	return smbHandleIdle
}

func (s *SMB) openFile(name string) (smbFile, error) {
	if s.openRO != nil {
		return s.openRO(name)
	}
	return s.share.Open(s.p(name))
}

// acquire returns an open handle of name. Only packs (which never change
// once written) are kept open; other objects are opened for one read.
func (s *SMB) acquire(name string) (*smbHandle, error) {
	c := &s.handles
	if !strings.HasPrefix(name, "data/") {
		f, err := s.openFile(name)
		if err != nil {
			return nil, err
		}
		return &smbHandle{f: f, refs: 1, evicted: true}, nil
	}
	c.mu.Lock()
	if h := c.open[name]; h != nil {
		h.refs++
		c.seq++
		h.used = c.seq
		c.mu.Unlock()
		return h, nil
	}
	c.mu.Unlock()
	f, err := s.openFile(name)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if h := c.open[name]; h != nil { // another read opened it meanwhile
		h.refs++
		c.mu.Unlock()
		_ = f.Close()
		return h, nil
	}
	c.seq++
	h := &smbHandle{f: f, refs: 1, used: c.seq, evicted: c.closed}
	var drop []smbFile
	if !c.closed {
		if c.open == nil {
			c.open = map[string]*smbHandle{}
		}
		c.open[name] = h
		for len(c.open) > smbHandleCache {
			oldName, old := "", (*smbHandle)(nil)
			for n, o := range c.open {
				if o != h && (old == nil || o.used < old.used) {
					oldName, old = n, o
				}
			}
			drop = c.dropLocked(oldName, old, drop)
		}
	}
	c.mu.Unlock()
	closeAll(drop)
	return h, nil
}

// dropLocked takes a handle out of the cache. An idle one is added to drop
// for the caller to close after unlocking; one in use is closed when its
// last read ends.
func (c *smbHandles) dropLocked(name string, h *smbHandle, drop []smbFile) []smbFile {
	delete(c.open, name)
	h.evicted = true
	if h.refs == 0 {
		drop = append(drop, h.f)
	}
	return drop
}

func closeAll(files []smbFile) {
	for _, f := range files {
		_ = f.Close()
	}
}

func (s *SMB) release(h *smbHandle) {
	c := &s.handles
	c.mu.Lock()
	h.refs--
	closeIt := h.refs == 0 && h.evicted
	if h.refs == 0 && !h.evicted {
		h.idle = time.Now()
		c.armSweepLocked()
	}
	c.mu.Unlock()
	if closeIt {
		_ = h.f.Close()
	}
}

// armSweepLocked makes sure a sweep will close the handles left idle.
func (c *smbHandles) armSweepLocked() {
	if c.sweeping || len(c.open) == 0 {
		return
	}
	c.sweeping = true
	time.AfterFunc(c.idleTime(), c.sweep)
}

func (c *smbHandles) sweep() {
	c.mu.Lock()
	c.sweeping = false
	var drop []smbFile
	for n, h := range c.open {
		if h.refs == 0 && time.Since(h.idle) >= c.idleTime() {
			drop = c.dropLocked(n, h, drop)
		}
	}
	c.armSweepLocked() // for the handles still in use or idle for less
	c.mu.Unlock()
	closeAll(drop)
}

// forget closes the handle of name (now, or when its last read ends).
func (s *SMB) forget(name string) {
	c := &s.handles
	c.mu.Lock()
	var drop []smbFile
	if h := c.open[name]; h != nil {
		drop = c.dropLocked(name, h, nil)
	}
	c.mu.Unlock()
	closeAll(drop)
}

// closeHandles closes every cached handle; reads still running close
// theirs when they end.
func (s *SMB) closeHandles() {
	c := &s.handles
	c.mu.Lock()
	c.closed = true
	var drop []smbFile
	for n, h := range c.open {
		drop = c.dropLocked(n, h, drop)
	}
	c.mu.Unlock()
	closeAll(drop)
}

// readWhole reads an open file: with one ReadAt when its size is known and
// not too large, else as a stream. limit < 0 means no limit.
func readWhole(f smbFile, name string, limit int64) ([]byte, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := fi.Size()
	if limit >= 0 && size > limit {
		return nil, tooLarge(name, limit)
	}
	if size < 0 || size > smbOneRead {
		r := io.NewSectionReader(f, 0, 1<<62)
		if limit >= 0 {
			return readMax(r, name, limit)
		}
		return io.ReadAll(r)
	}
	buf := make([]byte, size)
	n, err := f.ReadAt(buf, 0)
	if n == len(buf) {
		return buf, nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return nil, fmt.Errorf("read %s: %w", name, err)
}
