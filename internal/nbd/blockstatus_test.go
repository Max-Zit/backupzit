package nbd

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"reflect"
	"testing"
)

// fakeQEMU answers like QEMU's NBD server with one dirty bitmap: dirty
// holds the dirty byte ranges, answers are split at split bytes (servers
// may answer less than asked).
func fakeQEMU(t *testing.T, l net.Listener, size uint64, dirty []Area, split uint64) {
	c, err := l.Accept()
	if err != nil {
		return
	}
	defer c.Close()
	w := func(v ...any) {
		for _, x := range v {
			binary.Write(c, binary.BigEndian, x)
		}
	}
	rep := func(opt, typ uint32, data []byte) { w(uint64(replyMagicOpt), opt, typ, uint32(len(data)), data) }
	w(uint64(nbdMagic), uint64(optMagic), uint16(flagFixedNew|flagNoZeroes))
	var cflags uint32
	binary.Read(c, binary.BigEndian, &cflags)
	const ctxID = 7
	structured := false
	for {
		var magic uint64
		var opt, n uint32
		if binary.Read(c, binary.BigEndian, &magic) != nil {
			return
		}
		binary.Read(c, binary.BigEndian, &opt)
		binary.Read(c, binary.BigEndian, &n)
		data := make([]byte, n)
		io.ReadFull(c, data)
		switch opt {
		case optStructuredReply:
			structured = true
			rep(opt, repAck, nil)
		case optSetMetaContext:
			// export name, count, queries
			el := binary.BigEndian.Uint32(data)
			q := data[4+el+4:]
			ql := binary.BigEndian.Uint32(q)
			name := string(q[4 : 4+ql])
			if name == "qemu:dirty-bitmap:bz-1" && structured {
				rep(opt, repMetaContext, append(binary.BigEndian.AppendUint32(nil, ctxID), name...))
			}
			rep(opt, repAck, nil)
		case optGo:
			info := binary.BigEndian.AppendUint16(nil, infoExport)
			info = binary.BigEndian.AppendUint64(info, size)
			info = binary.BigEndian.AppendUint16(info, tflagHasFlags|tflagReadOnly)
			rep(opt, repInfo, info)
			rep(opt, repAck, nil)
			goto transmission
		default:
			rep(opt, repErrUnsup, nil)
		}
	}
transmission:
	for {
		var magic uint32
		var flags, typ uint16
		var handle, off uint64
		var length uint32
		if binary.Read(c, binary.BigEndian, &magic) != nil {
			return
		}
		binary.Read(c, binary.BigEndian, &flags)
		binary.Read(c, binary.BigEndian, &typ)
		binary.Read(c, binary.BigEndian, &handle)
		binary.Read(c, binary.BigEndian, &off)
		binary.Read(c, binary.BigEndian, &length)
		if typ == cmdDisc {
			return
		}
		if typ != cmdBlockStatus || off+uint64(length) > size {
			w(uint32(structuredReplyMagic), uint16(replyFlagDone), uint16(replyTypeErrorBit|1), handle, uint32(6), uint32(errInval), uint16(0))
			continue
		}
		end := off + uint64(min(uint64(length), split))
		// Descriptors: alternate clean/dirty extents over [off, end).
		payload := binary.BigEndian.AppendUint32(nil, ctxID)
		for pos := off; pos < end; {
			isDirty, next := false, end
			for _, d := range dirty {
				switch {
				case pos >= d.Offset && pos < d.Offset+d.Length:
					isDirty, next = true, min(next, d.Offset+d.Length)
				case d.Offset > pos:
					next = min(next, d.Offset)
				}
			}
			st := uint32(0)
			if isDirty {
				st = 1
			}
			payload = binary.BigEndian.AppendUint32(payload, uint32(next-pos))
			payload = binary.BigEndian.AppendUint32(payload, st)
			pos = next
		}
		// First an empty reply chunk of another kind, then the status.
		w(uint32(structuredReplyMagic), uint16(0), uint16(replyTypeNone), handle, uint32(0))
		w(uint32(structuredReplyMagic), uint16(replyFlagDone), uint16(replyTypeBlockStatus), handle, uint32(len(payload)), payload)
	}
}

func TestDirtyAreas(t *testing.T) {
	const size = 3<<30 + 4096 // more than one request
	dirty := []Area{{0, 65536}, {1 << 20, 3 << 16}, {1<<30 - 65536, 131072}, {3 << 30, 4096}}
	for _, split := range []uint64{blockStatusChunk, 1 << 29} {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go fakeQEMU(t, l, size, dirty, split)
		got, gotSize, err := DirtyAreas(context.Background(), l.Addr().String(), "drive-scsi0", "bz-1")
		l.Close()
		if err != nil {
			t.Fatal(err)
		}
		if gotSize != size || !reflect.DeepEqual(got, dirty) {
			t.Errorf("split %d: size %d areas %v", split, gotSize, got)
		}
	}
	// A bitmap the server does not have (the VM was restarted).
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	go fakeQEMU(t, l, size, dirty, blockStatusChunk)
	if _, _, err := DirtyAreas(context.Background(), l.Addr().String(), "drive-scsi0", "other"); err == nil {
		t.Fatal("missing bitmap not reported")
	}
}
