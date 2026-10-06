package server

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"
)

// A failed TLS dial must not leave a nil *tls.Conn behind (the next send
// crashed the server).
func TestSyslogSenderFailedTLS(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	x := &syslogSender{}
	x.configure(AuditSyslog{Address: addr, Protocol: "tls"})
	for i := 0; i < 2; i++ {
		if err := x.send("line"); err == nil {
			t.Fatal("send to a closed port succeeded")
		}
	}
}

// When the syslog server closes the connection (restart), the next entry
// goes over a new connection instead of being lost.
func TestSyslogSenderReconnect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 10)
	go func() {
		first := true
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			r := bufio.NewReader(c)
			line, _ := r.ReadString('\n')
			got <- strings.TrimSpace(line)
			if first { // the server restarts after the first message
				first = false
				c.Close()
				continue
			}
			go func() {
				for {
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					got <- strings.TrimSpace(l)
				}
			}()
		}
	}()
	x := &syslogSender{}
	x.configure(AuditSyslog{Address: ln.Addr().String(), Protocol: "tcp"})
	want := []string{"one", "two", "three"}
	for _, w := range want {
		if err := x.send(w + "\n"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, w := range want {
		select {
		case l := <-got:
			if !strings.HasSuffix(l, w) {
				t.Errorf("got %q, want %q", l, w)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%q was lost", w)
		}
	}
}
