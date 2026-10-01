// Package testutil provides helpers for tests, such as an in-process SFTP server.
package testutil

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// SFTPServer is an in-memory SFTP server listening on localhost.
type SFTPServer struct {
	Addr        string
	User        string
	Password    string
	Fingerprint string

	ln       net.Listener
	handlers sftp.Handlers
	wg       sync.WaitGroup
}

// StartSFTPServer starts a server with an in-memory filesystem.
func StartSFTPServer(user, password string) (*SFTPServer, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, err
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if c.User() == user && string(pw) == password {
				return nil, nil
			}
			return nil, errors.New("access denied")
		},
	}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &SFTPServer{
		Addr:        ln.Addr().String(),
		User:        user,
		Password:    password,
		Fingerprint: ssh.FingerprintSHA256(signer.PublicKey()),
		ln:          ln,
		handlers:    sftp.InMemHandler(),
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(nc, cfg)
		}
	}()
	return s, nil
}

// URL returns sftp://user@addr/path.
func (s *SFTPServer) URL(path string) string {
	return fmt.Sprintf("sftp://%s@%s/%s", s.User, s.Addr, path)
}

func (s *SFTPServer) serve(nc net.Conn, cfg *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		nc.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		if nch.ChannelType() != "session" {
			nch.Reject(ssh.UnknownChannelType, "unsupported")
			continue
		}
		ch, chReqs, err := nch.Accept()
		if err != nil {
			continue
		}
		go func() {
			for req := range chReqs {
				ok := req.Type == "subsystem" && len(req.Payload) > 4 && string(req.Payload[4:]) == "sftp"
				req.Reply(ok, nil)
				if ok {
					srv := sftp.NewRequestServer(ch, s.handlers)
					if err := srv.Serve(); err != nil && err != io.EOF {
						_ = err
					}
					srv.Close()
					ch.Close()
				}
			}
		}()
	}
}

// Close stops accepting connections.
func (s *SFTPServer) Close() error {
	err := s.ln.Close()
	s.wg.Wait()
	return err
}
