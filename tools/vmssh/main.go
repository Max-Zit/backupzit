// Command vmssh runs commands on test VMs over SSH with password or key
// authentication. Development tool for the test lab; not shipped.
//
//	vmssh -host 192.168.101.102 -user ai -pass ai -- uname -a
//	vmssh -host ... -key ~/.ssh/id_ed25519 -sudo -- apt-get update
//	vmssh -host ... -put local.file:/remote/path
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

func main() {
	host := flag.String("host", "", "host[:port]")
	user := flag.String("user", "", "username")
	pass := flag.String("pass", os.Getenv("VMSSH_PASS"), "password (also used for sudo)")
	keyFile := flag.String("key", "", "private key file")
	sudo := flag.Bool("sudo", false, "run the command with sudo (password from -pass)")
	put := flag.String("put", "", "upload local:remote before running the command")
	flag.Parse()

	if !strings.Contains(*host, ":") {
		*host += ":22"
	}
	var auths []ssh.AuthMethod
	if *keyFile != "" {
		b, err := os.ReadFile(*keyFile)
		check(err)
		s, err := ssh.ParsePrivateKey(b)
		check(err)
		auths = append(auths, ssh.PublicKeys(s))
	}
	if *pass != "" {
		auths = append(auths, ssh.Password(*pass))
	}
	c, err := ssh.Dial("tcp", *host, &ssh.ClientConfig{
		User: *user, Auth: auths, Timeout: 15 * time.Second,
		// Lab VMs are reinstalled often; host keys are not pinned here.
		HostKeyCallback: func(string, net.Addr, ssh.PublicKey) error { return nil },
	})
	check(err)
	defer c.Close()

	if *put != "" {
		i := strings.LastIndex(*put, ":")
		local, remote, ok := (*put)[:max(i, 0)], (*put)[i+1:], i > 0
		if !ok {
			check(fmt.Errorf("-put wants local:remote"))
		}
		sc, err := sftp.NewClient(c)
		check(err)
		src, err := os.Open(local)
		check(err)
		sc.MkdirAll(path.Dir(remote))
		dst, err := sc.Create(remote)
		check(err)
		_, err = io.Copy(dst, src)
		check(err)
		dst.Close()
		src.Close()
		sc.Close()
	}

	cmd := strings.Join(flag.Args(), " ")
	if cmd == "" {
		return
	}
	s, err := c.NewSession()
	check(err)
	defer s.Close()
	if *sudo {
		cmd = "sudo -S -p '' sh -c " + shellQuote(cmd)
		s.Stdin = bytes.NewBufferString(*pass + "\n")
	}
	s.Stdout, s.Stderr = os.Stdout, os.Stderr
	if err := s.Run(cmd); err != nil {
		if ee, ok := err.(*ssh.ExitError); ok {
			os.Exit(ee.ExitStatus())
		}
		check(err)
	}
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "vmssh:", err)
		os.Exit(1)
	}
}
