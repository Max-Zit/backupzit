package nbd

import (
	"bytes"
	"encoding/binary"
	"io"
	"math/rand"
	"net"
	"testing"
)

func writeAll(w io.Writer, v ...any) {
	for _, x := range v {
		binary.Write(w, binary.BigEndian, x)
	}
}

// client is a minimal NBD client for tests.
type client struct {
	c    net.Conn
	size uint64
}

func dial(t *testing.T, addr, export string) *client {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	var hello struct {
		Magic, Opt uint64
		Flags      uint16
	}
	binary.Read(c, binary.BigEndian, &hello)
	if hello.Magic != nbdMagic || hello.Opt != optMagic || hello.Flags&flagFixedNew == 0 {
		t.Fatalf("handshake %+v", hello)
	}
	binary.Write(c, binary.BigEndian, uint32(flagFixedNew|clientNoZeroes))
	// An unsupported option first (structured replies), then GO.
	writeAll(c, uint64(optMagic), uint32(8), uint32(0))
	var rep struct {
		Magic       uint64
		Opt, Typ, N uint32
	}
	binary.Read(c, binary.BigEndian, &rep)
	if rep.Typ != repErrUnsup {
		t.Fatalf("structured replies: %+v", rep)
	}
	data := make([]byte, 4+len(export)+2)
	binary.BigEndian.PutUint32(data, uint32(len(export)))
	copy(data[4:], export)
	binary.Write(c, binary.BigEndian, uint64(optMagic))
	binary.Write(c, binary.BigEndian, uint32(optGo))
	binary.Write(c, binary.BigEndian, uint32(len(data)))
	c.Write(data)
	cl := &client{c: c}
	for {
		binary.Read(c, binary.BigEndian, &rep)
		payload := make([]byte, rep.N)
		io.ReadFull(c, payload)
		if rep.Typ == repInfo && binary.BigEndian.Uint16(payload) == infoExport {
			cl.size = binary.BigEndian.Uint64(payload[2:])
			if binary.BigEndian.Uint16(payload[10:])&tflagReadOnly == 0 {
				t.Error("export not read-only")
			}
		}
		if rep.Typ == repAck {
			return cl
		}
		if rep.Typ&(1<<31) != 0 {
			t.Fatalf("GO failed: %x %s", rep.Typ, payload)
		}
	}
}

func (cl *client) request(typ uint16, handle, off uint64, n uint32, payload []byte) {
	writeAll(cl.c, uint32(requestMagic), uint16(0), typ, handle, off, n)
	if payload != nil {
		cl.c.Write(payload)
	}
}

func (cl *client) reply(n int) (uint32, uint64, []byte) {
	var r struct {
		Magic, Err uint32
		Handle     uint64
	}
	binary.Read(cl.c, binary.BigEndian, &r)
	var data []byte
	if r.Err == 0 && n > 0 {
		data = make([]byte, n)
		io.ReadFull(cl.c, data)
	}
	return r.Err, r.Handle, data
}

func TestServer(t *testing.T) {
	disk := make([]byte, 5<<20+123)
	rand.New(rand.NewSource(1)).Read(disk)
	s := NewServer(Export{Name: "disk0", Size: int64(len(disk)), Data: bytes.NewReader(disk)})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go s.Serve(l)
	cl := dial(t, l.Addr().String(), "disk0")
	if cl.size != uint64(len(disk)) {
		t.Fatalf("size %d", cl.size)
	}
	// Two reads in flight; replies are matched by handle.
	cl.request(cmdRead, 1, 4096, 1<<20, nil)
	cl.request(cmdRead, 2, uint64(len(disk))-100, 100, nil)
	got := map[uint64][]byte{}
	for i := 0; i < 2; i++ {
		n := 1 << 20
		errno, h, _ := uint32(0), uint64(0), []byte(nil)
		// Read the header first to know the length.
		var r struct {
			Magic, Err uint32
			Handle     uint64
		}
		binary.Read(cl.c, binary.BigEndian, &r)
		errno, h = r.Err, r.Handle
		if h == 2 {
			n = 100
		}
		data := make([]byte, n)
		io.ReadFull(cl.c, data)
		if errno != 0 {
			t.Fatalf("read %d: errno %d", h, errno)
		}
		got[h] = data
	}
	if !bytes.Equal(got[1], disk[4096:4096+1<<20]) || !bytes.Equal(got[2], disk[len(disk)-100:]) {
		t.Error("read data differs")
	}
	// Writes are refused, reads past the end are invalid.
	cl.request(cmdWrite, 3, 0, 4, []byte("evil"))
	if errno, h, _ := cl.reply(0); errno != errPerm || h != 3 {
		t.Errorf("write: errno %d handle %d", errno, h)
	}
	cl.request(cmdRead, 4, uint64(len(disk)), 1, nil)
	if errno, _, _ := cl.reply(0); errno != errInval {
		t.Errorf("read past end: errno %d", errno)
	}
	cl.request(cmdFlush, 5, 0, 0, nil)
	if errno, _, _ := cl.reply(0); errno != 0 {
		t.Errorf("flush: %d", errno)
	}
	cl.request(cmdDisc, 6, 0, 0, nil)
	cl.c.Close()
}
