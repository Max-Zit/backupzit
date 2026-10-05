package instantnfs

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	nfs "github.com/willscott/go-nfs"
)

// Port is the NFS port: ESXi always connects to 2049 and asks the
// portmapper (port 111) where the MOUNT service is.
const Port = 2049

// RPC programs.
const (
	progPortmap = 100000
	progNFS     = 100003
	progMount   = 100005
)

// Serve answers NFS and MOUNT on port 2049 until ctx ends. Connections from
// addresses the handler does not allow are closed at once. The services are
// registered with the machine's rpcbind; without one, a minimal portmapper
// is started on port 111.
func Serve(ctx context.Context, h *Handler, logf func(string, ...any)) error {
	l, err := net.Listen("tcp", fmt.Sprintf(":%d", Port))
	if err != nil {
		return err
	}
	if err := register(); err != nil {
		logf("no rpcbind (%v); answering portmapper requests on port 111", err)
		if err := servePortmap(ctx); err != nil {
			l.Close()
			return fmt.Errorf("portmapper: %w", err)
		}
	}
	go func() { <-ctx.Done(); l.Close() }()
	err = nfs.Serve(&filtered{Listener: l, h: h, logf: logf}, h)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// Unregister removes the services from rpcbind.
func Unregister() {
	for _, p := range []uint32{progNFS, progMount} {
		pmapCall(2, p, 3, 0) // UNSET
	}
}

type filtered struct {
	net.Listener
	h    *Handler
	logf func(string, ...any)
}

func (f *filtered) Accept() (net.Conn, error) {
	for {
		c, err := f.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if f.h.allowed(c) {
			return c, nil
		}
		f.logf("refused NFS connection from %s", c.RemoteAddr())
		c.Close()
	}
}

func register() error {
	for _, p := range []uint32{progNFS, progMount} {
		if err := pmapCall(1, p, 3, Port); err != nil { // SET
			return err
		}
	}
	return nil
}

// pmapCall sends a portmapper v2 SET (1) or UNSET (2) to the local rpcbind.
func pmapCall(proc, prog, vers, port uint32) error {
	c, err := net.DialTimeout("udp", "127.0.0.1:111", 2*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	var b bytes.Buffer
	for _, v := range []uint32{uint32(time.Now().UnixNano()), 0, 2, progPortmap, 2, proc, 0, 0, 0, 0, prog, vers, 6, port} {
		binary.Write(&b, binary.BigEndian, v)
	}
	if _, err := c.Write(b.Bytes()); err != nil {
		return err
	}
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	resp := make([]byte, 64)
	n, err := c.Read(resp)
	if err != nil {
		return err
	}
	// xid, REPLY, MSG_ACCEPTED, verf (2 words), SUCCESS, bool
	if n < 28 || binary.BigEndian.Uint32(resp[n-4:]) != 1 {
		return errors.New("rpcbind refused the registration")
	}
	return nil
}

// servePortmap answers GETPORT (3) for NFS and MOUNT v3 over UDP and TCP
// on port 111.
func servePortmap(ctx context.Context) error {
	uc, err := net.ListenPacket("udp", ":111")
	if err != nil {
		return err
	}
	tl, err := net.Listen("tcp", ":111")
	if err != nil {
		uc.Close()
		return err
	}
	go func() { <-ctx.Done(); uc.Close(); tl.Close() }()
	go func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := uc.ReadFrom(buf)
			if err != nil {
				return
			}
			if r := pmapReply(buf[:n]); r != nil {
				uc.WriteTo(r, addr)
			}
		}
	}()
	go func() {
		for {
			c, err := tl.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				c.SetDeadline(time.Now().Add(30 * time.Second))
				for {
					var mark [4]byte
					if _, err := io.ReadFull(c, mark[:]); err != nil {
						return
					}
					size := binary.BigEndian.Uint32(mark[:]) & 0x7fffffff
					if size > 4096 {
						return
					}
					req := make([]byte, size)
					if _, err := io.ReadFull(c, req); err != nil {
						return
					}
					r := pmapReply(req)
					if r == nil {
						return
					}
					binary.BigEndian.PutUint32(mark[:], uint32(len(r))|0x80000000)
					c.Write(append(mark[:], r...))
				}
			}(c)
		}
	}()
	return nil
}

// pmapReply answers a portmapper call: GETPORT for NFS/MOUNT v3 over TCP
// gives 2049, everything else 0; NULL succeeds.
func pmapReply(req []byte) []byte {
	if len(req) < 40 {
		return nil
	}
	u := func(i int) uint32 { return binary.BigEndian.Uint32(req[i*4:]) }
	xid, prog, proc := u(0), u(3), u(5)
	if u(1) != 0 || prog != progPortmap {
		return nil
	}
	// Skip credentials and verifier.
	off := 24
	for i := 0; i < 2; i++ {
		if len(req) < off+8 {
			return nil
		}
		off += 8 + int((binary.BigEndian.Uint32(req[off+4:])+3)&^3)
	}
	var out bytes.Buffer
	for _, v := range []uint32{xid, 1, 0, 0, 0, 0} {
		binary.Write(&out, binary.BigEndian, v)
	}
	switch proc {
	case 0: // NULL
	case 3: // GETPORT
		if len(req) < off+16 {
			return nil
		}
		p, vers, proto := binary.BigEndian.Uint32(req[off:]), binary.BigEndian.Uint32(req[off+4:]), binary.BigEndian.Uint32(req[off+8:])
		port := uint32(0)
		if (p == progNFS || p == progMount) && vers == 3 && proto == 6 {
			port = Port
		}
		binary.Write(&out, binary.BigEndian, port)
	default:
		binary.Write(&out, binary.BigEndian, uint32(0))
	}
	return out.Bytes()
}
