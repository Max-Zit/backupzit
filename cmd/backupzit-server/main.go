// Command backupzit-server runs the management console and agent API.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/backupzit/backupzit/internal/server"
	"github.com/backupzit/backupzit/internal/update"
	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func run() error {
	listen := flag.String("listen", env("BACKUPZIT_LISTEN", ":8443"), "HTTPS listen address")
	dbURL := flag.String("db", env("BACKUPZIT_DB", ""), "PostgreSQL URL, e.g. postgres://backupzit:pass@localhost/backupzit")
	dataDir := flag.String("data-dir", env("BACKUPZIT_DATA_DIR", defaultDataDir()), "directory for TLS certificate and agent downloads")
	publicURL := flag.String("public-url", env("BACKUPZIT_PUBLIC_URL", ""), "URL agents use to reach this server (default: as seen in the browser)")
	tlsHosts := flag.String("tls-hosts", env("BACKUPZIT_TLS_HOSTS", ""), "comma-separated DNS names/IPs for the generated certificate")
	devDB := flag.Bool("dev-embedded-db", false, "start a private PostgreSQL in the data directory (development/evaluation only)")
	setPassword := flag.String("set-admin-password", "", "set the password of user 'admin' and exit")
	unblock := flag.String("unblock-ip", "", "lift the sign-in block of an address (or \"all\") and exit")
	devHTTP := flag.String("dev-http", "", "also serve plain HTTP on this loopback address, e.g. 127.0.0.1:8080 (development only)")
	showVersion := flag.Bool("version", false, "print version and exit")
	applyUpdate := flag.Bool("apply-update", false, "install an update requested in the web console (run by the backupzit-update service as root) and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("backupzit-server", version)
		return nil
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *applyUpdate {
		return runApplyUpdate(ctx, *dataDir, *dbURL, *listen, log)
	}

	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		return err
	}
	if *devDB {
		pg, url, err := startEmbeddedDB(*dataDir)
		if err != nil {
			return fmt.Errorf("embedded database: %w", err)
		}
		defer pg.Stop()
		*dbURL = url
		log.Warn("using embedded development database", "dir", filepath.Join(*dataDir, "pgdata"))
	}
	if *dbURL == "" {
		return errors.New("database URL required (--db or BACKUPZIT_DB)")
	}
	pool, err := server.OpenDB(ctx, *dbURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	store := server.NewStore(pool)
	key, created, err := server.LoadOrCreateSecretKey(*dataDir)
	if err != nil {
		return fmt.Errorf("secrets key: %w", err)
	}
	if err := store.UseSecretKey(key); err != nil {
		return err
	}
	if created {
		log.Warn("created a new secrets key; back it up together with the database", "file", filepath.Join(*dataDir, server.SecretKeyFile))
	}
	if n, err := store.EncryptExistingSecrets(ctx); err != nil {
		return fmt.Errorf("encrypt stored secrets: %w", err)
	} else if n > 0 {
		log.Info("encrypted secrets stored in plaintext by an earlier version", "records", n)
	}

	if *setPassword != "" {
		if err := store.SetPassword(ctx, "admin", *setPassword); err != nil {
			return err
		}
		fmt.Println("password for 'admin' updated")
		return nil
	}
	if *unblock != "" {
		n, err := store.UnblockAddress(ctx, *unblock)
		if err != nil {
			return err
		}
		fmt.Printf("%d address(es) unblocked\n", n)
		return nil
	}
	if err := ensureAdmin(ctx, store, *dataDir, log); err != nil {
		return err
	}

	var hosts []string
	for _, h := range strings.Split(*tlsHosts, ",") {
		if h = strings.TrimSpace(h); h != "" {
			hosts = append(hosts, h)
		}
	}
	cert, err := server.LoadOrCreateCert(*dataDir, hosts)
	if err != nil {
		return fmt.Errorf("tls certificate: %w", err)
	}

	srv, err := server.New(store, log)
	if err != nil {
		return err
	}
	srv.CertFingerprint = server.CertFingerprint(cert)
	srv.PublicURL = strings.TrimRight(*publicURL, "/")
	srv.DistDir = filepath.Join(*dataDir, "dist")
	srv.Version = version
	os.MkdirAll(srv.DistDir, 0o755)

	srv.Notifier = server.NewNotifier(store, log, func() string { return srv.PublicURL })
	srv.StartWeb(ctx, *dataDir)
	srv.StartUpdates(ctx, *dataDir)
	sched := server.NewScheduler(store, log)
	sched.Notifier = srv.Notifier
	go sched.Run(ctx)

	hs := &http.Server{
		Addr:              *listen,
		Handler:           srv.Handler(),
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		<-ctx.Done()
		server.Shutdown(hs)
	}()
	if *devHTTP != "" {
		if host, _, _ := strings.Cut(*devHTTP, ":"); host != "127.0.0.1" && host != "localhost" {
			return errors.New("--dev-http only accepts a loopback address")
		}
		plain := &http.Server{Addr: *devHTTP, Handler: srv.Handler(), ReadHeaderTimeout: 15 * time.Second, ReadTimeout: 5 * time.Minute, IdleTimeout: 2 * time.Minute}
		go plain.ListenAndServe()
		go func() { <-ctx.Done(); server.Shutdown(plain) }()
		log.Warn("serving plain HTTP for development", "addr", *devHTTP)
	}
	log.Info("BackupZit server listening", "addr", *listen, "fingerprint", srv.CertFingerprint, "version", version)
	if err := hs.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func defaultDataDir() string {
	if os.PathSeparator == '\\' {
		return filepath.Join(env("ProgramData", `C:\ProgramData`), "backupzit-server")
	}
	return "/var/lib/backupzit"
}

// ensureAdmin creates the 'admin' user on first start. The password comes
// from BACKUPZIT_ADMIN_PASSWORD or is generated and written to a file.
func ensureAdmin(ctx context.Context, store *server.Store, dataDir string, log *slog.Logger) error {
	pw := os.Getenv("BACKUPZIT_ADMIN_PASSWORD")
	generated := pw == ""
	if generated {
		pw = server.RandomPassword()
	}
	created, err := store.EnsureAdmin(ctx, "admin", pw)
	if err != nil || !created {
		return err
	}
	if generated {
		p := filepath.Join(dataDir, "initial-admin-password.txt")
		if err := os.WriteFile(p, []byte(pw+"\n"), 0o600); err != nil {
			return err
		}
		log.Warn("created user 'admin' with a generated password", "password_file", p)
	} else {
		log.Info("created user 'admin' from BACKUPZIT_ADMIN_PASSWORD")
	}
	return nil
}

func startEmbeddedDB(dataDir string) (*embeddedpostgres.EmbeddedPostgres, string, error) {
	const port = 54321
	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Port(port).
		Username("backupzit").Password("backupzit").Database("backupzit").
		DataPath(filepath.Join(dataDir, "pgdata")).
		RuntimePath(filepath.Join(dataDir, "pgruntime")).
		BinariesPath(filepath.Join(dataDir, "pgbin")).
		Logger(nil))
	if err := pg.Start(); err != nil {
		return nil, "", err
	}
	return pg, fmt.Sprintf("postgres://backupzit:backupzit@localhost:%d/backupzit?sslmode=disable", port), nil
}

// runApplyUpdate installs an update the console requested (as root).
func runApplyUpdate(ctx context.Context, dataDir, dbURL, listen string, log *slog.Logger) error {
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		port = "8443"
	}
	a := &update.Applier{
		Dir: filepath.Join(dataDir, "update"), Work: "/var/lib/backupzit-update", Binary: bin, CurrentVersion: version,
		DBURL: dbURL, HealthURL: "https://127.0.0.1:" + port + "/login", Service: "backupzit-server",
		Log: func(format string, args ...any) { log.Info(fmt.Sprintf(format, args...)) },
	}
	res, err := a.Apply(ctx)
	if res == nil && err == nil {
		log.Info("no update requested")
	}
	return err
}
