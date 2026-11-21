package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/recovery"
	"golang.org/x/term"
)

// readPassphrase takes the access package passphrase from
// BACKUPZIT_ACCESS_PASSPHRASE or asks for it (twice when confirm is set).
func readPassphrase(confirm bool) (string, error) {
	if p := os.Getenv("BACKUPZIT_ACCESS_PASSPHRASE"); p != "" {
		return p, nil
	}
	fd := int(os.Stdin.Fd()) // #nosec G115 -- file descriptor
	if !term.IsTerminal(fd) {
		return "", errors.New("no passphrase: set BACKUPZIT_ACCESS_PASSPHRASE or run in a terminal")
	}
	fmt.Fprint(os.Stderr, "Access package passphrase: ")
	p1, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if confirm {
		fmt.Fprint(os.Stderr, "Repeat the passphrase: ")
		p2, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		if string(p1) != string(p2) {
			return "", errors.New("the passphrases do not match")
		}
	}
	return string(p1), nil
}

func cmdAccessPackage(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: backupzit-agent access-package create|show ...")
	}
	switch args[0] {
	case "create":
		return cmdAccessCreate(ctx, args[1:])
	case "show":
		return cmdAccessShow(ctx, args[1:])
	}
	return fmt.Errorf("unknown access-package command %q (create, show)", args[0])
}

func cmdAccessCreate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("access-package create", flag.ExitOnError)
	rf := addRepoFlags(fs)
	name := fs.String("name", "", "name of the repository in the package (default: last part of the location)")
	out := fs.String("out", "", "access package file to write ("+recovery.AccessPackageExt+"); an existing one gets the repository added")
	noTest := fs.Bool("no-test", false, "do not open the repository before adding it")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: backupzit-agent access-package create --repo <location> [credentials] --out <file"+recovery.AccessPackageExt+">")
		fmt.Fprintln(os.Stderr, "Stores the location and credentials encrypted with a passphrase of their own (BACKUPZIT_ACCESS_PASSPHRASE or prompt).")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if rf.location == "" || *out == "" {
		fs.Usage()
		return errors.New("--repo and --out are required")
	}
	if !strings.HasSuffix(strings.ToLower(*out), recovery.AccessPackageExt) {
		*out += recovery.AccessPackageExt
	}
	a := recovery.AccessRepo{Name: *name, Location: rf.location, Password: rf.password,
		SFTPPassword: rf.opts.SFTPPassword, SFTPHostKey: rf.opts.SFTPHostKey,
		S3AccessKey: rf.opts.S3AccessKey, S3SecretKey: rf.opts.S3SecretKey, S3Region: rf.opts.S3Region,
		SMBPassword: rf.opts.SMBPassword, SMBDomain: rf.opts.SMBDomain, AzureKey: rf.opts.AzureKey, AzureSAS: rf.opts.AzureSAS,
		HardenedKey: rf.opts.HardenedKey, HardenedFingerprint: rf.opts.HardenedFingerprint}
	if a.Name == "" {
		a.Name = filepath.Base(strings.TrimRight(strings.ReplaceAll(rf.location, `\`, "/"), "/"))
	}
	if rf.opts.SFTPKeyFile != "" {
		b, err := os.ReadFile(rf.opts.SFTPKeyFile)
		if err != nil {
			return err
		}
		a.SFTPKey = string(b)
	}
	if !*noTest {
		r, err := rf.open(ctx)
		if err != nil {
			return fmt.Errorf("cannot open the repository with these credentials (use --no-test to add it anyway): %w", err)
		}
		_ = r.Close()
	}

	var p *recovery.AccessPackage
	existing, err := os.ReadFile(*out)
	switch {
	case err == nil:
		pass, err := readPassphrase(false)
		if err != nil {
			return err
		}
		if p, err = recovery.OpenAccessPackage(existing, pass); err != nil {
			return err
		}
		kept := p.Repositories[:0]
		for _, r := range p.Repositories {
			if r.Name != a.Name {
				kept = append(kept, r)
			}
		}
		p.Repositories = append(kept, a)
		// The package now holds the current credentials.
		p.Created = time.Now().UTC()
		b, err := recovery.SealAccessPackage(p, pass)
		if err != nil {
			return err
		}
		return writeAccess(*out, b, len(p.Repositories))
	case errors.Is(err, os.ErrNotExist):
		pass, err := readPassphrase(true)
		if err != nil {
			return err
		}
		host, _ := os.Hostname()
		p = &recovery.AccessPackage{CreatedBy: host, Repositories: []recovery.AccessRepo{a}}
		b, err := recovery.SealAccessPackage(p, pass)
		if err != nil {
			return err
		}
		return writeAccess(*out, b, 1)
	default:
		return err
	}
}

func writeAccess(path string, b []byte, n int) error {
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return err
	}
	fmt.Printf("wrote %s (%d repositories). Copy it to \\backupzit\\ on the recovery medium.\n", path, n)
	return nil
}

func cmdAccessShow(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("access-package show", flag.ExitOnError)
	test := fs.Bool("test", false, "open every repository to check the credentials")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: backupzit-agent access-package show [--test] <file" + recovery.AccessPackageExt + ">")
	}
	b, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	pass, err := readPassphrase(false)
	if err != nil {
		return err
	}
	p, err := recovery.OpenAccessPackage(b, pass)
	if err != nil {
		return err
	}
	fmt.Printf("created %s by %s\n", p.Created.Local().Format("2006-01-02 15:04"), p.CreatedBy)
	failed := 0
	for _, r := range p.Repositories {
		state := ""
		if *test {
			src, cleanup, err := r.Source(os.TempDir())
			if err == nil {
				var s *recovery.Session
				if s, err = recovery.Open(ctx, src); err == nil {
					_ = s.Close()
				}
			}
			cleanup()
			state = "  ok"
			if err != nil {
				state = "  FAILED: " + err.Error()
				failed++
			}
		}
		fmt.Printf("  %-20s %s%s\n", r.Name, r.Location, state)
	}
	if failed > 0 {
		return fmt.Errorf("%d repositories could not be opened", failed)
	}
	return nil
}
