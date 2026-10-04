// Package nbd serves read-only disks over the Network Block Device
// protocol (fixed newstyle handshake), so a hypervisor can boot a VM
// directly from a backup: QEMU reads the disk from BackupZit and keeps its
// writes in a local overlay (instant recovery).
package nbd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
)

const (
	nbdMagic       = 0x4e42444d41474943 // "NBDMAGIC"
	optMagic       = 0x49484156454f5054 // "IHAVEOPT"
	replyMagicOpt  = 0x0003e889045565a9
	requestMagic   = 0x25609513
	simpleReply    = 0x67446698
	flagFixedNew   = 1 << 0
	flagNoZeroes   = 1 << 1
	clientNoZeroes = 1 << 1

	optExportName = 1
	optAbort      = 2
	optList       = 3
	optInfo       = 6
	optGo         = 7

	repAck        = 1
	repServer     = 2
	repInfo       = 3
	repErrUnsup   = 1<<31 + 1
	repErrUnknown = 1<<31 + 6

	infoExport = 0

	tflagHasFlags  = 1 << 0
	tflagReadOnly  = 1 << 1
	tflagSendFlush = 1 << 2
	tflagMultiConn = 1 << 8

	cmdRead  = 0
	cmdWrite = 1
	cmdDisc  = 2
	cmdFlush = 3
	cmdTrim  = 4

	errPerm  = 1
	errIO    = 5
	errInval = 22

	maxRequest = 32 << 20
)

// Export is a disk offered by the server.
type Export struct {
	Name string
	Size int64
	Data io.ReaderAt
}

// Server serves exports to NBD clients.
type Server struct {
	mu      sync.Mutex
	exports map[string]Export
	Log     func(format string, args ...any)
}

// NewServer offers the given exports.
func NewServer(exports ...Export) *Server {
	s := &Server{exports: map[string]Export{}}
	for _, e := range exports {
		s.exports[e.Name] = e
	}
	return s
}

func (s *Server) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log(format, args...)
	}
}

// Serve accepts connections until the listener is closed.
func (s *Server) Serve(l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go func() {
			defer c.Close()
			if err := s.handle(c); err != nil && !errors.Is(err, io.EOF) {
				s.logf("nbd client %s: %v", c.RemoteAddr(), err)
			}
		}()
	}
}

type conn struct {
	rw io.ReadWriter
}

func (c conn) write(v ...any) error {
	for _, x := range v {
		if err := binary.Write(c.rw, binary.BigEndian, x); err != nil {
			return err
		}
	}
	return nil
}

func (c conn) optReply(opt, typ uint32, data []byte) error {
	if err := c.write(uint64(replyMagicOpt), opt, typ, uint32(len(data))); err != nil {
		return err
	}
	_, err := c.rw.Write(data)
	return err
}

func (s *Server) handle(nc net.Conn) error {
	c := conn{nc}
	if err := c.write(uint64(nbdMagic), uint64(optMagic), uint16(flagFixedNew|flagNoZeroes)); err != nil {
		return err
	}
	var clientFlags uint32
	if err := binary.Read(nc, binary.BigEndian, &clientFlags); err != nil {
		return err
	}
	for {
		var hdr struct {
			Magic  uint64
			Option uint32
			Length uint32
		}
		if err := binary.Read(nc, binary.BigEndian, &hdr); err != nil {
			return err
		}
		if hdr.Magic != optMagic || hdr.Length > 4096 {
			return errors.New("bad option")
		}
		data := make([]byte, hdr.Length)
		if _, err := io.ReadFull(nc, data); err != nil {
			return err
		}
		switch hdr.Option {
		case optExportName:
			e, ok := s.export(string(data))
			if !ok {
				return fmt.Errorf("unknown export %q", data)
			}
			if err := c.write(uint64(e.Size), uint16(tflagHasFlags|tflagReadOnly|tflagSendFlush|tflagMultiConn)); err != nil {
				return err
			}
			if clientFlags&clientNoZeroes == 0 {
				nc.Write(make([]byte, 124))
			}
			return s.transmit(nc, e)
		case optInfo, optGo:
			if len(data) < 4 {
				c.optReply(hdr.Option, repErrUnknown, nil)
				continue
			}
			nl := binary.BigEndian.Uint32(data)
			if int(nl) > len(data)-4 {
				return errors.New("bad export name")
			}
			e, ok := s.export(string(data[4 : 4+nl]))
			if !ok {
				c.optReply(hdr.Option, repErrUnknown, []byte("unknown export"))
				continue
			}
			info := make([]byte, 12)
			binary.BigEndian.PutUint16(info[0:], infoExport)
			binary.BigEndian.PutUint64(info[2:], uint64(e.Size))
			binary.BigEndian.PutUint16(info[10:], tflagHasFlags|tflagReadOnly|tflagSendFlush|tflagMultiConn)
			if err := c.optReply(hdr.Option, repInfo, info); err != nil {
				return err
			}
			if err := c.optReply(hdr.Option, repAck, nil); err != nil {
				return err
			}
			if hdr.Option == optGo {
				return s.transmit(nc, e)
			}
		case optList:
			s.mu.Lock()
			for name := range s.exports {
				b := make([]byte, 4+len(name))
				binary.BigEndian.PutUint32(b, uint32(len(name)))
				copy(b[4:], name)
				c.optReply(hdr.Option, repServer, b)
			}
			s.mu.Unlock()
			c.optReply(hdr.Option, repAck, nil)
		case optAbort:
			c.optReply(hdr.Option, repAck, nil)
			return nil
		default:
			// Structured replies, metadata contexts, TLS: not offered.
			if err := c.optReply(hdr.Option, repErrUnsup, nil); err != nil {
				return err
			}
		}
	}
}

func (s *Server) export(name string) (Export, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if name == "" && len(s.exports) == 1 {
		for _, e := range s.exports {
			return e, true
		}
	}
	e, ok := s.exports[name]
	return e, ok
}

// transmit answers block requests of one client.
func (s *Server) transmit(nc net.Conn, e Export) error {
	var wmu sync.Mutex
	reply := func(errno uint32, handle uint64, data []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		hdr := make([]byte, 16)
		binary.BigEndian.PutUint32(hdr[0:], simpleReply)
		binary.BigEndian.PutUint32(hdr[4:], errno)
		binary.BigEndian.PutUint64(hdr[8:], handle)
		if _, err := nc.Write(hdr); err != nil {
			return err
		}
		if len(data) > 0 {
			_, err := nc.Write(data)
			return err
		}
		return nil
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	sem := make(chan struct{}, 8) // parallel reads per client
	for {
		var req struct {
			Magic  uint32
			Flags  uint16
			Type   uint16
			Handle uint64
			Offset uint64
			Length uint32
		}
		if err := binary.Read(nc, binary.BigEndian, &req); err != nil {
			return err
		}
		if req.Magic != requestMagic {
			return errors.New("bad request magic")
		}
		switch req.Type {
		case cmdRead:
			if req.Length > maxRequest || req.Offset+uint64(req.Length) > uint64(e.Size) {
				if err := reply(errInval, req.Handle, nil); err != nil {
					return err
				}
				continue
			}
			sem <- struct{}{}
			wg.Add(1)
			go func(off uint64, n uint32, handle uint64) {
				defer func() { <-sem; wg.Done() }()
				buf := make([]byte, n)
				if _, err := e.Data.ReadAt(buf, int64(off)); err != nil && !errors.Is(err, io.EOF) {
					s.logf("nbd read at %d: %v", off, err)
					reply(errIO, handle, nil)
					return
				}
				reply(0, handle, buf)
			}(req.Offset, req.Length, req.Handle)
		case cmdWrite:
			// Discard the payload; the export is read-only.
			if _, err := io.CopyN(io.Discard, nc, int64(req.Length)); err != nil {
				return err
			}
			if err := reply(errPerm, req.Handle, nil); err != nil {
				return err
			}
		case cmdFlush, cmdTrim:
			if err := reply(0, req.Handle, nil); err != nil {
				return err
			}
		case cmdDisc:
			return nil
		default:
			if err := reply(errInval, req.Handle, nil); err != nil {
				return err
			}
		}
	}
}
