// Package vmware backs up and restores virtual machines of VMware ESXi
// hosts without agents in the guests. Any BackupZit agent can act as the
// proxy: it talks to the host's vSphere API (inventory, changed block
// tracking, snapshots on licensed hosts), reads virtual disks over HTTPS
// from the datastore and uses SSH for the operations the free ESXi license
// does not allow through the API (snapshots, creating VMs, writing disks).
package vmware

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/license"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/soap"
	"golang.org/x/crypto/ssh"
)

// Conn describes how to reach an ESXi host.
type Conn struct {
	// Host is the address of the ESXi host (name or IP, optional :port).
	Host     string `json:"host"`
	User     string `json:"user"`
	Password string `json:"password,omitempty"`
	// Thumbprint pins the host's HTTPS certificate ("SHA256:…"). When
	// empty, the certificate presented is accepted and reported in
	// Client.Thumbprint so the caller can pin it.
	Thumbprint string `json:"thumbprint,omitempty"`
	// SSHHostKey pins the SSH host key ("SHA256:…"), like Thumbprint.
	SSHHostKey string `json:"ssh_host_key,omitempty"`
	// SSHPort defaults to 22.
	SSHPort int `json:"ssh_port,omitempty"`
}

// Client is a connection to one ESXi host.
type Client struct {
	conn Conn
	vim  *vim25.Client
	dc   *object.Datacenter
	find *find.Finder

	// Thumbprint and SSHHostKey are the fingerprints the host presented.
	Thumbprint string
	SSHHostKey string
	// Version is the ESXi version, e.g. "8.0.3".
	Version string
	// APIWrites is false on hosts with the free license, whose API is
	// read-only; snapshots and VM changes then go through SSH.
	APIWrites bool

	sshMu sync.Mutex
	ssh   *ssh.Client
}

// Fingerprint formats the SHA-256 fingerprint of a DER certificate.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// VMIDFor derives the BackupZit number of a VM from its instance UUID.
func VMIDFor(uuid string) int {
	h := fnv.New32a()
	h.Write([]byte("vmware:" + strings.ToLower(uuid)))
	return 1_000_000_000 + int(h.Sum32()%1_000_000_000)
}

func hostPort(h string, port int) string {
	if _, _, err := net.SplitHostPort(h); err == nil {
		return h
	}
	return net.JoinHostPort(strings.Trim(h, "[]"), fmt.Sprint(port))
}

// Connect signs in to the host's API.
func Connect(ctx context.Context, conn Conn) (*Client, error) {
	if conn.Host == "" || conn.User == "" {
		return nil, errors.New("ESXi host and user are required")
	}
	u, err := url.Parse("https://" + hostPort(conn.Host, 443) + "/sdk")
	if err != nil {
		return nil, err
	}
	c := &Client{conn: conn}
	sc := soap.NewClient(u, true)
	tr := sc.DefaultTransport()
	tr.TLSClientConfig.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
		if len(raw) == 0 {
			return errors.New("no certificate")
		}
		fp := Fingerprint(raw[0])
		c.Thumbprint = fp
		if conn.Thumbprint != "" && fp != conn.Thumbprint {
			return fmt.Errorf("the ESXi certificate changed (%s, expected %s); if the host was reinstalled, confirm the new certificate", fp, conn.Thumbprint)
		}
		return nil
	}
	tr.TLSClientConfig.MinVersion = tls.VersionTLS12
	vc, err := vim25.NewClient(ctx, sc)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", conn.Host, err)
	}
	if err := session.NewManager(vc).Login(ctx, url.UserPassword(conn.User, conn.Password)); err != nil {
		return nil, fmt.Errorf("sign in to %s: %w", conn.Host, err)
	}
	c.vim = vc
	c.Version = vc.ServiceContent.About.Version
	if vc.ServiceContent.About.ApiType != "HostAgent" {
		c.Logout()
		return nil, errors.New("connect BackupZit to the ESXi host itself; vCenter is not supported yet")
	}
	c.find = find.NewFinder(vc, true)
	if c.dc, err = c.find.DefaultDatacenter(ctx); err != nil {
		c.Logout()
		return nil, err
	}
	c.find.SetDatacenter(c.dc)
	c.APIWrites = apiWritable(ctx, vc)
	return c, nil
}

// apiWritable reports whether the host's license allows API changes: the
// free "vSphere Hypervisor" license makes the API read-only.
func apiWritable(ctx context.Context, vc *vim25.Client) bool {
	ls, err := license.NewManager(vc).List(ctx)
	if err != nil {
		return false
	}
	for _, l := range ls {
		if strings.HasPrefix(l.EditionKey, "esx.hypervisor.") {
			return false
		}
	}
	return true
}

// Logout ends the API session and closes SSH.
func (c *Client) Logout() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if c.vim != nil {
		session.NewManager(c.vim).Logout(ctx)
	}
	c.sshMu.Lock()
	if c.ssh != nil {
		c.ssh.Close()
		c.ssh = nil
	}
	c.sshMu.Unlock()
}

// sshClient connects (once) to the host's SSH service.
func (c *Client) sshClient() (*ssh.Client, error) {
	c.sshMu.Lock()
	defer c.sshMu.Unlock()
	if c.ssh != nil {
		return c.ssh, nil
	}
	port := c.conn.SSHPort
	if port == 0 {
		port = 22
	}
	pw := c.conn.Password
	cfg := &ssh.ClientConfig{
		User: c.conn.User,
		Auth: []ssh.AuthMethod{
			ssh.Password(pw),
			// ESXi asks for the password with keyboard-interactive.
			ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) {
				ans := make([]string, len(qs))
				for i := range ans {
					ans[i] = pw
				}
				return ans, nil
			}),
		},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			fp := ssh.FingerprintSHA256(key)
			c.SSHHostKey = fp
			if c.conn.SSHHostKey != "" && fp != c.conn.SSHHostKey {
				return fmt.Errorf("the ESXi SSH host key changed (%s, expected %s)", fp, c.conn.SSHHostKey)
			}
			return nil
		},
		Timeout: 20 * time.Second,
	}
	host, _, err := net.SplitHostPort(hostPort(c.conn.Host, 443))
	if err != nil {
		return nil, err
	}
	cl, err := ssh.Dial("tcp", hostPort(host, port), cfg)
	if err != nil {
		return nil, fmt.Errorf("SSH to %s (enable SSH on the ESXi host: Host → Actions → Services → Enable Secure Shell): %w", host, err)
	}
	c.ssh = cl
	return cl, nil
}

// run executes a shell command on the host and returns its output.
func (c *Client) run(ctx context.Context, cmd string) (string, error) {
	cl, err := c.sshClient()
	if err != nil {
		return "", err
	}
	s, err := cl.NewSession()
	if err != nil {
		return "", err
	}
	defer s.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			s.Signal(ssh.SIGKILL)
			s.Close()
		case <-done:
		}
	}()
	out, err := s.CombinedOutput(cmd)
	if err != nil {
		return string(out), fmt.Errorf("%s: %w: %s", strings.Fields(cmd)[0], err, strings.TrimSpace(lastLines(string(out), 3)))
	}
	return string(out), nil
}

func lastLines(s string, n int) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, " ")
}

// sq quotes a string for the ESXi shell.
func sq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// CheckSSH verifies that SSH works (needed for snapshots on free hosts
// and for restores).
func (c *Client) CheckSSH(ctx context.Context) error {
	out, err := c.run(ctx, "vmware -v")
	if err != nil {
		return err
	}
	if !strings.Contains(out, "ESXi") {
		return fmt.Errorf("unexpected answer over SSH: %q", strings.TrimSpace(out))
	}
	return nil
}
