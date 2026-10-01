// Command backupzit-repo is the BackupZit hardened repository: an HTTPS
// service on a Linux server that stores backups write-once and immutable.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/backupzit/backupzit/internal/hardened"
	"github.com/backupzit/backupzit/internal/tlsutil"
)

var version = "dev"

func usage() {
	fmt.Fprint(os.Stderr, `backupzit-repo - BackupZit hardened repository

Usage:
  backupzit-repo serve                 run the service (systemd does this)
  backupzit-repo add-key <name>        create an access key for a BackupZit console / agents
  backupzit-repo keys                  list access keys
  backupzit-repo remove-key <name>     revoke an access key
  backupzit-repo fingerprint           show the TLS certificate fingerprint clients pin
  backupzit-repo status                show stored and deleted (still retained) data
  backupzit-repo undelete --since TIME restore files deleted at or after TIME
  backupzit-repo version

Common flags (or environment variables):
  --data DIR         storage directory (BACKUPZIT_REPO_DATA, default /srv/backupzit)
  --config-dir DIR   keys and certificate (BACKUPZIT_REPO_CONFIG, default /etc/backupzit-repo)
`)
}

type config struct {
	data, configDir string
}

func (c *config) flags(fs *flag.FlagSet) {
	fs.StringVar(&c.data, "data", envOr("BACKUPZIT_REPO_DATA", "/srv/backupzit"), "storage directory")
	fs.StringVar(&c.configDir, "config-dir", envOr("BACKUPZIT_REPO_CONFIG", "/etc/backupzit-repo"), "configuration directory")
}

func (c *config) keys() *hardened.Keys { return hardened.NewKeys(filepath.Join(c.configDir, "keys")) }

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = cmdServe(args)
	case "add-key":
		err = cmdAddKey(args)
	case "keys":
		err = cmdKeys(args)
	case "remove-key":
		err = cmdRemoveKey(args)
	case "fingerprint":
		err = cmdFingerprint(args)
	case "status":
		err = cmdStatus(args)
	case "undelete":
		err = cmdUndelete(args)
	case "version":
		fmt.Println("backupzit-repo", version)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var c config
	c.flags(fs)
	listen := fs.String("listen", envOr("BACKUPZIT_REPO_LISTEN", ":8500"), "listen address")
	lockDays := fs.Int("lock-days", atoi(envOr("BACKUPZIT_REPO_LOCK_DAYS", "30")), "immutability period in days for every stored file")
	hosts := fs.String("tls-hosts", os.Getenv("BACKUPZIT_REPO_TLS_HOSTS"), "comma-separated names/IPs for the self-signed certificate")
	fs.Parse(args)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	store, err := hardened.OpenStore(c.data, *lockDays)
	if err != nil {
		return err
	}
	if err := store.CheckImmutable(); err != nil {
		return fmt.Errorf("immutable attribute not available: %w", err)
	}
	cert, err := tlsutil.LoadOrCreateCert(c.configDir, splitList(*hosts))
	if err != nil {
		return err
	}
	keys := c.keys()
	if names, _ := keys.Names(); len(names) == 0 {
		log.Warn("no access keys yet; create one with: backupzit-repo add-key <name>")
	}
	srv := &http.Server{
		Addr:              *listen,
		Handler:           (&hardened.Server{Store: store, Keys: keys, Log: log, Version: version}).Handler(),
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       30 * time.Minute, // one upload of up to 4 GiB
		IdleTimeout:       5 * time.Minute,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			if n, err := store.Sweep(); err != nil {
				log.Error("sweep", "err", err)
			} else if n > 0 {
				log.Info("removed deleted files whose retention ended", "files", n)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	log.Info("BackupZit hardened repository listening", "addr", *listen, "data", c.data,
		"lock_days", *lockDays, "fingerprint", tlsutil.CertFingerprint(cert), "version", version)
	if err := srv.ListenAndServeTLS("", ""); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func cmdAddKey(args []string) error {
	fs := flag.NewFlagSet("add-key", flag.ExitOnError)
	var c config
	c.flags(fs)
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: backupzit-repo add-key <name>")
	}
	if err := os.MkdirAll(c.configDir, 0o700); err != nil {
		return err
	}
	token, err := c.keys().Add(fs.Arg(0))
	if err != nil {
		return err
	}
	fixOwner(filepath.Join(c.configDir, "keys"))
	fmt.Printf("Access key %q created. It is shown only once:\n\n  %s\n\n", fs.Arg(0), token)
	if cert, err := tls.LoadX509KeyPair(filepath.Join(c.configDir, "cert.pem"), filepath.Join(c.configDir, "key.pem")); err == nil {
		fmt.Printf("Certificate fingerprint: %s\n", tlsutil.CertFingerprint(cert))
	}
	return nil
}

func cmdKeys(args []string) error {
	fs := flag.NewFlagSet("keys", flag.ExitOnError)
	var c config
	c.flags(fs)
	fs.Parse(args)
	names, err := c.keys().Names()
	if err != nil {
		return err
	}
	for _, n := range names {
		fmt.Println(n)
	}
	if len(names) == 0 {
		fmt.Println("no access keys")
	}
	return nil
}

func cmdRemoveKey(args []string) error {
	fs := flag.NewFlagSet("remove-key", flag.ExitOnError)
	var c config
	c.flags(fs)
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: backupzit-repo remove-key <name>")
	}
	if err := c.keys().Remove(fs.Arg(0)); err != nil {
		return err
	}
	fixOwner(filepath.Join(c.configDir, "keys"))
	fmt.Printf("Access key %q revoked.\n", fs.Arg(0))
	return nil
}

func cmdFingerprint(args []string) error {
	fs := flag.NewFlagSet("fingerprint", flag.ExitOnError)
	var c config
	c.flags(fs)
	hosts := fs.String("tls-hosts", os.Getenv("BACKUPZIT_REPO_TLS_HOSTS"), "names/IPs for a new certificate")
	fs.Parse(args)
	cert, err := tlsutil.LoadOrCreateCert(c.configDir, splitList(*hosts))
	if err != nil {
		return err
	}
	fixOwner(filepath.Join(c.configDir, "cert.pem"), filepath.Join(c.configDir, "key.pem"))
	fmt.Println(tlsutil.CertFingerprint(cert))
	return nil
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	var c config
	c.flags(fs)
	fs.Parse(args)
	store, err := hardened.OpenStore(c.data, 1)
	if err != nil {
		return err
	}
	st, err := store.Stats()
	if err != nil {
		return err
	}
	fmt.Printf("stored:  %d files, %s\n", st.Files, human(st.Bytes))
	fmt.Printf("deleted: %d files, %s (kept until their retention ends; see undelete)\n", st.Hidden, human(st.HiddenBytes))
	return nil
}

func cmdUndelete(args []string) error {
	fs := flag.NewFlagSet("undelete", flag.ExitOnError)
	var c config
	c.flags(fs)
	since := fs.String("since", "", "restore files deleted at or after this time (e.g. 2026-10-01T14:30)")
	fs.Parse(args)
	if *since == "" {
		return errors.New("--since is required")
	}
	t, err := parseTime(*since)
	if err != nil {
		return err
	}
	store, err := hardened.OpenStore(c.data, 1)
	if err != nil {
		return err
	}
	n, err := store.Undelete(t)
	if err != nil {
		return err
	}
	fixOwner(filepath.Join(c.data, ".backupzit", "deleted.log"))
	fmt.Printf("%d files restored.\n", n)
	return nil
}

func parseTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid time %q (use e.g. 2026-10-01T14:30)", s)
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func human(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
