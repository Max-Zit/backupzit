package localipc

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// PipeName is the agent's local pipe.
const PipeName = `\\.\pipe\backupzit-agent`

// pipeSDDL: SYSTEM and administrators have full access, interactively
// logged-on users may read the status and start backups.
const pipeSDDL = "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GRGW;;;IU)"

// Listen creates the pipe server.
func Listen() (net.Listener, error) {
	sd, err := windows.SecurityDescriptorFromString(pipeSDDL)
	if err != nil {
		return nil, err
	}
	l := &pipeListener{sa: &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd},
		conns: make(chan net.Conn), done: make(chan struct{})}
	// Create the first instance now so that errors (e.g. a second agent)
	// are reported to the caller.
	h, err := l.create(true)
	if err != nil {
		return nil, err
	}
	go l.loop(h)
	return l, nil
}

type pipeListener struct {
	sa    *windows.SecurityAttributes
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func (l *pipeListener) create(first bool) (windows.Handle, error) {
	mode := uint32(windows.PIPE_ACCESS_DUPLEX)
	if first {
		mode |= windows.FILE_FLAG_FIRST_PIPE_INSTANCE
	}
	name, _ := windows.UTF16PtrFromString(PipeName)
	return windows.CreateNamedPipe(name, mode,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
		windows.PIPE_UNLIMITED_INSTANCES, 64<<10, 64<<10, 0, l.sa)
}

func (l *pipeListener) loop(h windows.Handle) {
	for {
		err := windows.ConnectNamedPipe(h, nil)
		select {
		case <-l.done:
			windows.CloseHandle(h)
			return
		default:
		}
		if err != nil && !errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
			windows.CloseHandle(h)
		} else {
			select {
			case l.conns <- &pipeConn{File: os.NewFile(uintptr(h), PipeName), h: h}:
			case <-l.done:
				windows.CloseHandle(h)
				return
			}
		}
		for {
			if h, err = l.create(false); err == nil {
				break
			}
			select {
			case <-l.done:
				return
			case <-time.After(time.Second):
			}
		}
	}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() {
		close(l.done)
		// Unblock ConnectNamedPipe with a connection of our own.
		if c, err := openPipe(); err == nil {
			c.Close()
		}
	})
	return nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr{} }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return PipeName }

// pipeConn is one connection; reads and writes are synchronous, deadlines
// are not supported (the requests are tiny).
type pipeConn struct {
	*os.File
	h      windows.Handle
	server bool
}

func (c *pipeConn) Close() error {
	windows.FlushFileBuffers(c.h)
	return c.File.Close()
}
func (c *pipeConn) LocalAddr() net.Addr                { return pipeAddr{} }
func (c *pipeConn) RemoteAddr() net.Addr               { return pipeAddr{} }
func (c *pipeConn) SetDeadline(t time.Time) error      { return nil }
func (c *pipeConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *pipeConn) SetWriteDeadline(t time.Time) error { return nil }

func openPipe() (*pipeConn, error) {
	name, _ := windows.UTF16PtrFromString(PipeName)
	h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return nil, err
	}
	return &pipeConn{File: os.NewFile(uintptr(h), PipeName), h: h}, nil
}

func dial(ctx context.Context) (net.Conn, error) {
	for {
		c, err := openPipe()
		if err == nil {
			return c, nil
		}
		if !errors.Is(err, windows.ERROR_PIPE_BUSY) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
