package localipc

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The decisions mirror the Windows pipe ACL: root everything, signed-in
// users status and run, other accounts nothing.
func TestAllowedCommand(t *testing.T) {
	const staff = 20
	cases := []struct {
		name string
		p    peer
		cmd  string
		want bool
	}{
		{"root status", peer{uid: 0}, CmdStatus, true},
		{"root other command", peer{uid: 0}, "shutdown", true},
		{"user status", peer{uid: 501, groups: []uint32{staff, 12, 80}}, CmdStatus, true},
		{"user run", peer{uid: 501, groups: []uint32{staff}}, CmdRun, true},
		{"user staff not first", peer{uid: 502, groups: []uint32{502, staff}}, CmdStatus, true},
		{"user other command", peer{uid: 501, groups: []uint32{staff}}, "shutdown", false},
		{"daemon account", peer{uid: 70, groups: []uint32{70}}, CmdStatus, false},
		{"no groups", peer{uid: 501}, CmdStatus, false},
	}
	for _, c := range cases {
		if got := allowedCommand(c.p, c.cmd, staff); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// Only the groups the kernel filled in count.
func TestPeerFromXucred(t *testing.T) {
	c := &unix.Xucred{Uid: 501, Ngroups: 2, Groups: [16]uint32{20, 12, 99}}
	p := peerFromXucred(c)
	if p.uid != 501 || len(p.groups) != 2 || p.groups[1] != 12 {
		t.Fatalf("%+v", p)
	}
	c.Ngroups = 40
	if p := peerFromXucred(c); len(p.groups) != 16 {
		t.Fatalf("overflow: %d groups", len(p.groups))
	}
}

// The kernel reports this test process as the peer of a socket it
// connected to itself, and Serve answers it according to its identity.
func TestServeChecksPeer(t *testing.T) {
	dir, err := os.MkdirTemp("", "bzipc")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "s")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Serve(ctx, l, func(context.Context, Request) Response { return Response{OK: true} })

	ask := func(cmd string) Response {
		c, err := net.DialTimeout("unix", path, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Write([]byte(`{"cmd":"` + cmd + `"}` + "\n")); err != nil {
			t.Fatal(err)
		}
		var resp Response
		buf := make([]byte, 4096)
		n, _ := c.Read(buf)
		if err := json.Unmarshal(buf[:n], &resp); err != nil {
			t.Fatalf("%q: %v", buf[:n], err)
		}
		return resp
	}
	me := peer{uid: uint32(os.Getuid())} // #nosec G115 -- uids fit 32 bits
	gs, _ := os.Getgroups()
	for _, g := range gs {
		me.groups = append(me.groups, uint32(g)) // #nosec G115 -- gids fit 32 bits
	}
	for _, cmd := range []string{CmdStatus, "shutdown"} {
		want := allowedCommand(me, cmd, staffGID())
		if got := ask(cmd).OK; got != want {
			t.Errorf("%s as uid %d: OK=%v, want %v", cmd, me.uid, got, want)
		}
	}
}
