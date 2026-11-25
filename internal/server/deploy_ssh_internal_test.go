package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// fakeSSH is an SSH server that answers the commands of a Linux agent
// installation the way a machine with the given sudo set-up would, and
// records every command with the input it received (#67).
type fakeSSH struct {
	t        *testing.T
	uid      string // "0" for root
	sudo     string // "nopasswd", "password" or "none"
	sudoPass string // the password sudo accepts
	echoBack bool   // the install prints the password (a broken or hostile script)

	mu   sync.Mutex
	runs []fakeRun
	file string // the script written by the mktemp step
}

type fakeRun struct{ cmd, stdin string }

func (f *fakeSSH) handle(cmd, stdin string) (string, int) {
	f.mu.Lock()
	f.runs = append(f.runs, fakeRun{cmd, stdin})
	f.mu.Unlock()
	switch {
	case cmd == "sh -s" && strings.HasPrefix(stdin, "hostname;"):
		return "web1\nx86_64\ndeb\n" + f.uid + "\n", 0
	case cmd == "sudo -n true":
		switch f.sudo {
		case "nopasswd":
			return "", 0
		case "password":
			return "sudo: a password is required\n", 1
		}
		return "user is not in the sudoers file.\n", 1
	case cmd == "sh -s" || cmd == "sudo -n sh -s":
		if f.uid != "0" && (cmd == "sh -s" || f.sudo != "nopasswd") {
			return "not root\n", 1
		}
		return f.script(stdin)
	case strings.HasPrefix(cmd, `sh -c 'umask 077; f=$(mktemp)`):
		f.mu.Lock()
		f.file = stdin
		f.mu.Unlock()
		return "/tmp/tmp.Ab12Cd34\n", 0
	case strings.HasPrefix(cmd, `sh -c 'rc=0; sudo -S -p "" sh /tmp/tmp.Ab12Cd34 `):
		if stdin != f.sudoPass+"\n" {
			return "Sorry, try again.\nsudo: 1 incorrect password attempt\n", 1
		}
		f.mu.Lock()
		script := f.file
		f.mu.Unlock()
		if !strings.HasPrefix(script, "exec </dev/null\n") {
			f.t.Errorf("the script file does not close its input: %q", script[:min(40, len(script))])
		}
		return f.script(strings.TrimPrefix(script, "exec </dev/null\n"))
	}
	f.t.Errorf("unexpected command %q", cmd)
	return "unknown\n", 127
}

// script answers the two root scripts: the enrollment lookup and the
// installation.
func (f *fakeSSH) script(s string) (string, int) {
	if strings.HasPrefix(s, "sed -n") {
		return "", 0
	}
	out := "package verified\npackage installed\nagent started\n"
	if f.echoBack {
		out = "sh: 1: " + f.sudoPass + ": not found\n" + out
	}
	return out, 0
}

func (f *fakeSSH) serve(t *testing.T) string {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	hk, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback:  func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) { return nil, nil },
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) { return nil, nil },
	}
	cfg.AddHostKey(hk)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			nc, err := l.Accept()
			if err != nil {
				return
			}
			go f.conn(nc, cfg)
		}
	}()
	return l.Addr().String()
}

func (f *fakeSSH) conn(nc net.Conn, cfg *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		if nch.ChannelType() != "session" {
			nch.Reject(ssh.UnknownChannelType, "")
			continue
		}
		ch, creqs, err := nch.Accept()
		if err != nil {
			continue
		}
		go func() {
			for req := range creqs {
				if req.Type != "exec" {
					req.Reply(false, nil)
					continue
				}
				var p struct{ Cmd string }
				ssh.Unmarshal(req.Payload, &p)
				req.Reply(true, nil)
				in, _ := io.ReadAll(ch)
				out, rc := f.handle(p.Cmd, string(in))
				ch.Write([]byte(out))
				ch.CloseWrite()
				ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(rc)}))
				ch.Close()
			}
		}()
	}
}

func TestDeployLinuxSudo(t *testing.T) {
	dist := t.TempDir()
	os.WriteFile(filepath.Join(dist, "backupzit-agent_0.61.0_amd64.deb"), []byte("deb"), 0o644)
	s := &Server{DistDir: dist}

	_, kpriv, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKeyWithPassphrase(kpriv, "", []byte("key-phrase-9"))
	if err != nil {
		t.Fatal(err)
	}
	encKey := string(pem.EncodeToMemory(block))

	const pw = `p'a"s$(id);|&>` + "`x`"
	for _, tc := range []struct {
		name    string
		fake    *fakeSSH
		q       deployRequest
		wantErr string
		sudoIn  string // the only input that may carry a password
	}{
		{name: "root", fake: &fakeSSH{uid: "0"}, q: deployRequest{User: "root", Password: pw}},
		{name: "nopasswd sudo, password login", fake: &fakeSSH{uid: "1000", sudo: "nopasswd"}, q: deployRequest{User: "ops", Password: pw}},
		{name: "nopasswd sudo, key with passphrase", fake: &fakeSSH{uid: "1000", sudo: "nopasswd"}, q: deployRequest{User: "ops", Key: encKey, KeyPassphrase: "key-phrase-9"}},
		{name: "password sudo", fake: &fakeSSH{uid: "1000", sudo: "password", sudoPass: pw, echoBack: true}, q: deployRequest{User: "ops", Password: pw}, sudoIn: pw + "\n"},
		{name: "key, own sudo password", fake: &fakeSSH{uid: "1000", sudo: "password", sudoPass: "sudo-pw-7"}, q: deployRequest{User: "ops", Key: encKey, KeyPassphrase: "key-phrase-9", SudoPassword: "sudo-pw-7"}, sudoIn: "sudo-pw-7\n"},
		{name: "key, sudo asks, none given", fake: &fakeSSH{uid: "1000", sudo: "password"}, q: deployRequest{User: "ops", Key: encKey, KeyPassphrase: "key-phrase-9"}, wantErr: "enter it in Sudo password"},
		{name: "wrong sudo password", fake: &fakeSSH{uid: "1000", sudo: "password", sudoPass: "right-one"}, q: deployRequest{User: "ops", Password: pw}, wantErr: "installation failed", sudoIn: pw + "\n"},
		{name: "no sudo rights", fake: &fakeSSH{uid: "1000", sudo: "none"}, q: deployRequest{User: "ops", Password: pw}, wantErr: "cannot use sudo"},
		{name: "key passphrase missing", fake: &fakeSSH{uid: "1000", sudo: "nopasswd"}, q: deployRequest{User: "ops", Key: encKey, Password: "login-pw-5"}, wantErr: "enter it in Key passphrase"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.fake
			f.t = t
			host, port, _ := net.SplitHostPort(f.serve(t))
			q := tc.q
			q.Host, q.OS = host, "linux"
			q.Port, _ = strconv.Atoi(port)
			if err := q.validate(); err != nil {
				t.Fatal(err)
			}
			d := &deployment{secrets: q.secrets()}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			_, err := s.deployLinux(ctx, d, q, "https://console:8443", "CODE-123")
			if tc.wantErr == "" && err != nil {
				t.Fatalf("install: %v (log %q)", err, d.view().Log)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("error %v, want %q", err, tc.wantErr)
			}
			secrets := q.secrets()
			f.mu.Lock()
			runs := append([]fakeRun(nil), f.runs...)
			f.mu.Unlock()
			for _, r := range runs {
				for _, x := range secrets {
					if strings.Contains(r.cmd, x) {
						t.Errorf("a secret in the command %q", r.cmd)
					}
					if strings.Contains(r.stdin, x) && r.stdin != tc.sudoIn {
						t.Errorf("a secret in the input of %q: %q", r.cmd, r.stdin)
					}
				}
				if tc.sudoIn == "" && strings.Contains(r.cmd, "sudo -S") {
					t.Errorf("a password was offered to sudo: %q", r.cmd)
				}
			}
			for _, l := range d.view().Log {
				for _, x := range secrets {
					if strings.Contains(l, x) {
						t.Errorf("a secret in the log line %q", l)
					}
				}
				if strings.Contains(l, "CODE-123") {
					t.Errorf("the enrollment code in the log line %q", l)
				}
			}
			if err != nil {
				for _, x := range secrets {
					if strings.Contains(redact(err.Error(), d.secrets), x) {
						t.Errorf("a secret in the error %v", err)
					}
				}
			}
		})
	}
}

func TestDeploySecretsRedacted(t *testing.T) {
	q := deployRequest{Password: "abc", SudoPassword: "abcdef", KeyPassphrase: " spaced "}
	d := &deployment{secrets: q.secrets()}
	d.logf("x %s y %s z %s", "abcdef", "abc", "spaced")
	if l := d.view().Log[0]; strings.Contains(l, "abc") || strings.Contains(l, "spaced") {
		t.Errorf("not hidden: %q", l)
	}
	for _, bad := range []deployRequest{{Host: "h", OS: "linux", User: "u", Password: "a\nb"}, {Host: "h", OS: "linux", User: "u", Key: "k", SudoPassword: "a\rb"}} {
		if bad.validate() == nil {
			t.Errorf("accepted a line break: %+v", bad)
		}
	}
}
