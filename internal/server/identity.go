package server

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/max-zit/backupzit/internal/vmware"
)

// Confirming a machine's identity before credentials are sent (B3, 0.62).
// When the form gives no SSH host key or certificate fingerprint, the
// console first reads the ones the machine presents (without signing in),
// shows them and continues only after the user confirmed them; the
// connection that sends the password then accepts exactly those. Before,
// the first connection trusted whatever answered, password included.

// pendingTTL is how long a shown fingerprint waits for the confirmation.
const pendingTTL = 10 * time.Minute

// identityPrint is one fingerprint shown for confirmation.
type identityPrint struct {
	Label string // English, translated on the page
	Value string
}

// pendingConnect is a connection waiting for the user to confirm the
// machine's identity. It holds the form values, password included, in
// memory only (never on a page) until then.
type pendingConnect struct {
	By      string
	Expires time.Time
	Prints  []identityPrint
	Deploy  *deployRequest
	VMware  *VMwareHost
	Via     bool // ESXi through the proxy agent
}

type pendingStore struct {
	mu sync.Mutex
	m  map[string]*pendingConnect
}

func (s *Server) pendings() *pendingStore {
	s.pendingOnce.Do(func() { s.pending = &pendingStore{m: map[string]*pendingConnect{}} })
	return s.pending
}

// add keeps p and returns its ID. It is dropped when it expires, also when
// nobody confirms it and no other is added (LOW: an abandoned confirmation
// kept its passwords in memory until the next one).
func (ps *pendingStore) add(p *pendingConnect) string {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	now := time.Now()
	ps.purge(now)
	id := randomID(12)
	p.Expires = now.Add(pendingTTL)
	ps.m[id] = p
	time.AfterFunc(pendingTTL+time.Second, func() {
		ps.mu.Lock()
		defer ps.mu.Unlock()
		ps.purge(time.Now())
	})
	return id
}

// purge drops the expired confirmations. ps.mu is held.
func (ps *pendingStore) purge(now time.Time) {
	for k, x := range ps.m {
		if now.After(x.Expires) {
			delete(ps.m, k)
		}
	}
}

// take removes and returns the pending connection id of user by.
func (ps *pendingStore) take(id, by string) (*pendingConnect, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.purge(time.Now())
	p, ok := ps.m[id]
	if !ok || p.By != by || time.Now().After(p.Expires) {
		return nil, errors.New("the confirmation expired; enter the details again")
	}
	delete(ps.m, id)
	return p, nil
}

// errGotKey stops an SSH handshake once the host key was read.
var errGotKey = errors.New("host key read")

// fetchSSHHostKey reads the SSH host key fingerprint of addr (host:port)
// without signing in. It prefers ed25519, as the deploy connection does.
func fetchSSHHostKey(ctx context.Context, addr string) (string, error) {
	var fp string
	cfg := &ssh.ClientConfig{User: "backupzit", Timeout: 15 * time.Second,
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			fp = ssh.FingerprintSHA256(key)
			return errGotKey
		}}
	d := net.Dialer{Timeout: 15 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	c, _, _, err := ssh.NewClientConn(conn, addr, cfg)
	if c != nil {
		_ = c.Close()
	}
	if fp != "" {
		return fp, nil
	}
	if err == nil {
		err = errors.New("no host key")
	}
	return "", err
}

// fetchTLSFingerprint reads the fingerprint ("SHA256:…") of the
// certificate addr (host:port) presents, without sending anything else.
func fetchTLSFingerprint(ctx context.Context, addr string) (string, error) {
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: 15 * time.Second},
		Config: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}} // #nosec G402 -- only reads the certificate to show it for confirmation
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	cs := conn.(*tls.Conn).ConnectionState()
	if len(cs.PeerCertificates) == 0 {
		return "", errors.New("no certificate")
	}
	return vmware.Fingerprint(cs.PeerCertificates[0].Raw), nil
}

// pinnedTLSDial dials addr with TLS and accepts only the certificate with
// fingerprint want (self-signed certificates of WinRM included).
func pinnedTLSDial(want string) func(network, addr string) (net.Conn, error) {
	return func(network, addr string) (net.Conn, error) {
		host, _, _ := net.SplitHostPort(addr)
		cfg := &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12, ServerName: host, // #nosec G402 -- the certificate is checked against the confirmed fingerprint below
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 {
					return errors.New("no certificate")
				}
				if fp := vmware.Fingerprint(cs.PeerCertificates[0].Raw); fp != want {
					return errors.New("the certificate " + fp + " does not match the confirmed " + want)
				}
				return nil
			}}
		return tls.DialWithDialer(&net.Dialer{Timeout: 30 * time.Second}, network, addr, cfg)
	}
}

// renderConfirmIdentity shows the fingerprints of a pending connection.
func (s *Server) renderConfirmIdentity(w http.ResponseWriter, r *http.Request, user string, p *pendingConnect, action, back, hint string) {
	id := s.pendings().add(p)
	s.render(w, r, "confirm_identity", pageData{Title: "Check the machine's identity", Nav: "agents", User: user, Data: map[string]any{
		"ID": id, "Prints": p.Prints, "Action": action, "Back": back, "Hint": hint}})
}
