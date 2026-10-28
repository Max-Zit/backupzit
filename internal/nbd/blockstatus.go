package nbd

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// The client side: QEMU serves a running VM's disk with a dirty bitmap as
// the metadata context "qemu:dirty-bitmap:NAME"; asking for the block
// status of the disk returns the areas written since the bitmap was
// created (changed block tracking for Proxmox VE backups). Only block
// status is used, no data is read.

const (
	optStructuredReply = 8
	optSetMetaContext  = 10
	repMetaContext     = 4

	cmdBlockStatus = 7

	structuredReplyMagic = 0x668e33ef
	replyFlagDone        = 1 << 0
	replyTypeNone        = 0
	replyTypeBlockStatus = 5
	replyTypeErrorBit    = 1 << 15

	// blockStatusChunk is how much is asked for per request (the length
	// is 32 bits).
	blockStatusChunk = 1 << 30
)

// Area is a byte range of a disk.
type Area struct{ Offset, Length uint64 }

// DirtyAreas connects to the NBD server at addr ("unix:/path" or a TCP
// address), opens export and returns the areas marked in the dirty bitmap
// named bitmap, and the size of the export.
func DirtyAreas(ctx context.Context, addr, export, bitmap string) ([]Area, uint64, error) {
	network, address := "tcp", addr
	if len(addr) > 5 && addr[:5] == "unix:" {
		network, address = "unix", addr[5:]
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, network, address)
	if err != nil {
		return nil, 0, err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	} else {
		c.SetDeadline(time.Now().Add(10 * time.Minute))
	}
	cl := &blockClient{r: bufio.NewReader(c), w: c}
	ctxName := "qemu:dirty-bitmap:" + bitmap
	size, ctxID, err := cl.handshake(export, ctxName)
	if err != nil {
		return nil, 0, err
	}
	var areas []Area
	for off := uint64(0); off < size; {
		n := uint32(min(size-off, blockStatusChunk))
		got, covered, err := cl.blockStatus(off, n, ctxID)
		if err != nil {
			return nil, 0, fmt.Errorf("block status at %d: %w", off, err)
		}
		if covered == 0 {
			return nil, 0, fmt.Errorf("block status at %d: empty answer", off)
		}
		for _, a := range got {
			if k := len(areas); k > 0 && areas[k-1].Offset+areas[k-1].Length == a.Offset {
				areas[k-1].Length += a.Length
			} else {
				areas = append(areas, a)
			}
		}
		off += covered
	}
	cl.write(uint32(requestMagic), uint16(0), uint16(cmdDisc), uint64(0), uint64(0), uint32(0))
	return areas, size, nil
}

type blockClient struct {
	r      *bufio.Reader
	w      io.Writer
	handle uint64
}

func (c *blockClient) write(v ...any) error {
	for _, x := range v {
		if err := binary.Write(c.w, binary.BigEndian, x); err != nil {
			return err
		}
	}
	return nil
}

func (c *blockClient) read(v ...any) error {
	for _, x := range v {
		if err := binary.Read(c.r, binary.BigEndian, x); err != nil {
			return err
		}
	}
	return nil
}

func (c *blockClient) option(opt uint32, data []byte) error {
	return c.write(uint64(optMagic), opt, uint32(len(data)), data)
}

// optReply reads one reply to option opt.
func (c *blockClient) optReply(opt uint32) (typ uint32, data []byte, err error) {
	var magic uint64
	var o, n uint32
	if err := c.read(&magic, &o, &typ, &n); err != nil {
		return 0, nil, err
	}
	if magic != replyMagicOpt || o != opt {
		return 0, nil, fmt.Errorf("unexpected option reply (magic %#x, option %d)", magic, o)
	}
	if n > 1<<20 {
		return 0, nil, errors.New("option reply too long")
	}
	data = make([]byte, n)
	if _, err := io.ReadFull(c.r, data); err != nil {
		return 0, nil, err
	}
	if typ&(1<<31) != 0 {
		return typ, data, fmt.Errorf("server refused option %d (%#x): %s", opt, typ, data)
	}
	return typ, data, nil
}

// handshake negotiates structured replies and the metadata context, and
// opens the export.
func (c *blockClient) handshake(export, metaContext string) (size uint64, ctxID uint32, err error) {
	var magic, opt uint64
	var flags uint16
	if err := c.read(&magic, &opt, &flags); err != nil {
		return 0, 0, fmt.Errorf("handshake: %w", err)
	}
	if magic != nbdMagic || opt != optMagic || flags&flagFixedNew == 0 {
		return 0, 0, errors.New("not an NBD server with the fixed newstyle handshake")
	}
	cflags := uint32(flagFixedNew)
	if flags&flagNoZeroes != 0 {
		cflags |= clientNoZeroes
	}
	if err := c.write(cflags); err != nil {
		return 0, 0, err
	}
	// Structured replies: block status needs them.
	if err := c.option(optStructuredReply, nil); err != nil {
		return 0, 0, err
	}
	if typ, _, err := c.optReply(optStructuredReply); err != nil || typ != repAck {
		return 0, 0, fmt.Errorf("structured replies: %v", err)
	}
	// The dirty bitmap as metadata context.
	var b []byte
	b = binary.BigEndian.AppendUint32(b, uint32(len(export)))
	b = append(b, export...)
	b = binary.BigEndian.AppendUint32(b, 1)
	b = binary.BigEndian.AppendUint32(b, uint32(len(metaContext)))
	b = append(b, metaContext...)
	if err := c.option(optSetMetaContext, b); err != nil {
		return 0, 0, err
	}
	found := false
	for {
		typ, data, err := c.optReply(optSetMetaContext)
		if err != nil {
			return 0, 0, err
		}
		if typ == repAck {
			break
		}
		if typ == repMetaContext && len(data) >= 4 && string(data[4:]) == metaContext {
			ctxID, found = binary.BigEndian.Uint32(data), true
		}
	}
	if !found {
		return 0, 0, fmt.Errorf("the server has no %s", metaContext)
	}
	// Open the export.
	b = binary.BigEndian.AppendUint32(nil, uint32(len(export)))
	b = append(b, export...)
	b = binary.BigEndian.AppendUint16(b, 0) // no extra information requests
	if err := c.option(optGo, b); err != nil {
		return 0, 0, err
	}
	for {
		typ, data, err := c.optReply(optGo)
		if err != nil {
			return 0, 0, err
		}
		if typ == repAck {
			break
		}
		if typ == repInfo && len(data) >= 12 && binary.BigEndian.Uint16(data) == infoExport {
			size = binary.BigEndian.Uint64(data[2:])
		}
	}
	if size == 0 {
		return 0, 0, errors.New("the export has no size")
	}
	return size, ctxID, nil
}

// blockStatus asks for the status of length bytes at off and returns the
// dirty areas and how many bytes the answer covered.
func (c *blockClient) blockStatus(off uint64, length uint32, ctxID uint32) ([]Area, uint64, error) {
	c.handle++
	if err := c.write(uint32(requestMagic), uint16(0), uint16(cmdBlockStatus), c.handle, off, length); err != nil {
		return nil, 0, err
	}
	var areas []Area
	var covered uint64
	for {
		var magic uint32
		var flags, typ uint16
		var handle uint64
		var n uint32
		if err := c.read(&magic, &flags, &typ, &handle, &n); err != nil {
			return nil, 0, err
		}
		if magic == simpleReply {
			return nil, 0, errors.New("server sent a simple reply to block status")
		}
		if magic != structuredReplyMagic || handle != c.handle {
			return nil, 0, fmt.Errorf("unexpected reply (magic %#x)", magic)
		}
		if n > 64<<20 {
			return nil, 0, errors.New("reply too long")
		}
		data := make([]byte, n)
		if _, err := io.ReadFull(c.r, data); err != nil {
			return nil, 0, err
		}
		switch {
		case typ&replyTypeErrorBit != 0:
			code := uint32(0)
			if len(data) >= 4 {
				code = binary.BigEndian.Uint32(data)
			}
			return nil, 0, fmt.Errorf("server error %d", code)
		case typ == replyTypeBlockStatus:
			if len(data) < 4 || (len(data)-4)%8 != 0 {
				return nil, 0, errors.New("malformed block status")
			}
			if binary.BigEndian.Uint32(data) != ctxID {
				break // another context
			}
			pos := off
			for d := data[4:]; len(d) >= 8; d = d[8:] {
				l, status := uint64(binary.BigEndian.Uint32(d)), binary.BigEndian.Uint32(d[4:])
				if status&1 != 0 {
					areas = append(areas, Area{Offset: pos, Length: l})
				}
				pos += l
			}
			covered = max(covered, pos-off)
		case typ == replyTypeNone:
		default:
			return nil, 0, fmt.Errorf("unexpected reply type %d", typ)
		}
		if flags&replyFlagDone != 0 {
			return areas, covered, nil
		}
	}
}
