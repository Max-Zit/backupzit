package testutil

import (
	"bufio"
	"net"
	"strings"
	"sync"
)

// Mail is a message received by SMTPServer.
type Mail struct {
	From string
	To   []string
	Data string
}

// SMTPServer is a minimal plain SMTP server that records messages.
type SMTPServer struct {
	Addr string
	ln   net.Listener
	mu   sync.Mutex
	mail []Mail
}

// StartSMTPServer listens on localhost.
func StartSMTPServer() (*SMTPServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &SMTPServer{Addr: ln.Addr().String(), ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s, nil
}

// Messages returns a copy of the received mail.
func (s *SMTPServer) Messages() []Mail {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Mail(nil), s.mail...)
}

// Close stops the server.
func (s *SMTPServer) Close() { s.ln.Close() }

func (s *SMTPServer) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	say := func(l string) { c.Write([]byte(l + "\r\n")) }
	say("220 test ESMTP")
	var m Mail
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			say("250 test")
		case strings.HasPrefix(cmd, "MAIL FROM:"):
			m = Mail{From: strings.Trim(strings.TrimSpace(line)[10:], "<> ")}
			say("250 ok")
		case strings.HasPrefix(cmd, "RCPT TO:"):
			m.To = append(m.To, strings.Trim(strings.TrimSpace(line)[8:], "<> "))
			say("250 ok")
		case cmd == "DATA":
			say("354 go ahead")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			m.Data = b.String()
			s.mu.Lock()
			s.mail = append(s.mail, m)
			s.mu.Unlock()
			say("250 queued")
		case cmd == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}
