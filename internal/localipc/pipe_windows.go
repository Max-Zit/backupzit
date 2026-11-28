package localipc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// PipeName is the agent's local pipe.
const PipeName = `\\.\pipe\backupzit-agent`

// pipeName is PipeName (tests use pipes of their own).
var pipeName = PipeName

// pipeSDDL: SYSTEM and administrators have full access, interactively
// logged-on users may read the status and start backups: they may read
// and write data (0x12008b = FILE_GENERIC_READ | FILE_WRITE_DATA), but not
// FILE_APPEND_DATA, which for a pipe is FILE_CREATE_PIPE_INSTANCE. With
// GENERIC_WRITE (up to 0.62) a user could add a server instance of his own
// to the agent's pipe and answer other users' trays with a made-up status
// and console address (security review MED 16).
const pipeSDDL = "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;0x12008b;;;IU)"

// clientAccess is what the tray and "backupzit-agent status" open the
// pipe with: no more than pipeSDDL grants users.
const clientAccess = windows.FILE_GENERIC_READ | windows.FILE_WRITE_DATA

// Listen creates the pipe server.
func Listen() (net.Listener, error) {
	sd, err := windows.SecurityDescriptorFromString(pipeSDDL)
	if err != nil {
		return nil, err
	}
	l := &pipeListener{sa: &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd},
		name: pipeName, conns: make(chan net.Conn), done: make(chan struct{})}
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
	name  string
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func (l *pipeListener) create(first bool) (windows.Handle, error) {
	mode := uint32(windows.PIPE_ACCESS_DUPLEX)
	if first {
		mode |= windows.FILE_FLAG_FIRST_PIPE_INSTANCE
	}
	name, _ := windows.UTF16PtrFromString(l.name)
	return windows.CreateNamedPipe(name, mode,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
		windows.PIPE_UNLIMITED_INSTANCES, 64<<10, 64<<10, 0, l.sa)
}

func (l *pipeListener) loop(h windows.Handle) {
	for {
		err := windows.ConnectNamedPipe(h, nil)
		select {
		case <-l.done:
			_ = windows.CloseHandle(h)
			return
		default:
		}
		// The next instance is made before this one is handed on (and
		// closed): a pipe whose last instance closes is gone, and any user
		// could then create it under the same name (MED 16).
		next, nerr := l.create(false)
		if err != nil && !errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
			_ = windows.CloseHandle(h)
		} else {
			select {
			case l.conns <- &pipeConn{File: os.NewFile(uintptr(h), l.name), h: h}:
			case <-l.done:
				_ = windows.CloseHandle(h)
				if nerr == nil {
					_ = windows.CloseHandle(next)
				}
				return
			}
		}
		for nerr != nil {
			select {
			case <-l.done:
				return
			case <-time.After(time.Second):
			}
			next, nerr = l.create(false)
		}
		h = next
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
		if c, err := openPipe(l.name); err == nil {
			_ = c.Close()
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
	h windows.Handle
}

func (c *pipeConn) Close() error {
	_ = windows.FlushFileBuffers(c.h)
	return c.File.Close()
}
func (c *pipeConn) LocalAddr() net.Addr                { return pipeAddr{} }
func (c *pipeConn) RemoteAddr() net.Addr               { return pipeAddr{} }
func (c *pipeConn) SetDeadline(t time.Time) error      { return nil }
func (c *pipeConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *pipeConn) SetWriteDeadline(t time.Time) error { return nil }

// openPipe connects to the pipe. The server learns who connects but may
// not act as that user (SECURITY_IDENTIFICATION): a program posing as the
// agent cannot use the tray user's rights.
func openPipe(pipe string) (*pipeConn, error) {
	name, _ := windows.UTF16PtrFromString(pipe)
	h, err := windows.CreateFile(name, clientAccess, 0, nil, windows.OPEN_EXISTING,
		windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION, 0)
	if err != nil {
		return nil, err
	}
	return &pipeConn{File: os.NewFile(uintptr(h), pipe), h: h}, nil
}

// trustedPipeOwner reports whether a pipe with this owner was made by the
// agent service: the owner of a pipe is the account that created its
// first instance (SYSTEM for the service, Administrators for an elevated
// "service run"), and no other user can set those. Tests also accept
// their own account.
var trustedPipeOwner = func(sid *windows.SID) bool {
	return sid.IsWellKnown(windows.WinLocalSystemSid) || sid.IsWellKnown(windows.WinBuiltinAdministratorsSid)
}

// checkServer refuses a pipe the agent service did not create: any user
// can create the pipe while the agent is not running (restart, update)
// and answer with a made-up status (MED 16). With pipeSDDL nobody else
// can add an instance to the agent's own pipe, so its owner tells who
// serves it.
func checkServer(h windows.Handle) error {
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("check the owner of %s: %w", pipeName, err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return fmt.Errorf("check the owner of %s: %w", pipeName, err)
	}
	if owner == nil || !trustedPipeOwner(owner) {
		who := "unknown"
		if owner != nil {
			who = owner.String()
			if acc, dom, _, err := owner.LookupAccount(""); err == nil {
				who = dom + `\` + acc
			}
		}
		return fmt.Errorf("%s was not created by the BackupZit agent service but by %s; another program may be posing as the agent", pipeName, who)
	}
	return nil
}

func dial(ctx context.Context) (net.Conn, error) {
	for {
		c, err := openPipe(pipeName)
		if err == nil {
			if err := checkServer(c.h); err != nil {
				_ = c.File.Close()
				return nil, err
			}
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

// authorize lets every caller through: the pipe's ACL (pipeSDDL) already
// limits who may connect, and every allowed caller may use every command.
func authorize(net.Conn, string) error { return nil }
