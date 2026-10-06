package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kardianos/service"
	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/api"
)

const serviceName = "backupzit-agent"

func cmdEnroll(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	server := fs.String("server", "", "management server URL, e.g. https://backup.example.com:8443")
	token := fs.String("token", "", "enrollment token from the console")
	fp := fs.String("fingerprint", "", "server certificate fingerprint SHA256:... from the console")
	cfgPath := fs.String("config", agent.DefaultConfigPath(), "agent configuration file")
	force := fs.Bool("force", false, "replace an existing enrollment")
	ifNot := fs.Bool("if-not-enrolled", false, "do nothing if the agent is already enrolled (used by installers)")
	code := fs.String("code", "", "enrollment code from the console (BZ1-...), instead of --server/--token/--fingerprint")
	pending := fs.Bool("pending-ok", false, "if the console cannot be reached now, keep the code and let the service retry (used by installers)")
	fs.Parse(args)
	if *code != "" {
		s, t, f, err := api.DecodeEnrollCode(*code)
		if err != nil {
			return err
		}
		*server, *token, *fp = s, t, f
	}
	if *server == "" || *token == "" || *fp == "" {
		fs.Usage()
		return errors.New("--code, or --server, --token and --fingerprint are required")
	}
	if old, err := agent.LoadConfig(*cfgPath); err == nil && !*force {
		switch {
		case *ifNot && old.Fingerprint == *fp:
			// A reinstall or upgrade with the same code (GPO, RMM) keeps
			// the enrollment instead of adding the machine again.
			fmt.Println("agent is already enrolled with this console, keeping existing enrollment")
			return nil
		case *ifNot:
			// The code is for another console: the administrator moves the
			// machine, so the new code wins.
			fmt.Printf("agent was enrolled with %s; enrolling with %s\n", old.ServerURL, *server)
		default:
			return fmt.Errorf("agent is already enrolled (%s); use --force to enroll again", *cfgPath)
		}
	}
	cfg, err := agent.Enroll(ctx, *server, *token, *fp, version)
	if err != nil {
		// Installers run enroll without a console; keep the reason on disk.
		os.MkdirAll(filepath.Dir(*cfgPath), 0o700)
		if f, ferr := os.OpenFile(filepath.Join(filepath.Dir(*cfgPath), "enroll-error.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); ferr == nil {
			fmt.Fprintf(f, "%s enroll with %s failed: %v\n", time.Now().Format(time.RFC3339), *server, err)
			f.Close()
		}
		if *pending && !strings.Contains(err.Error(), "invalid or expired enrollment token") {
			if perr := agent.SavePendingEnrollment(*cfgPath, *server, *token, *fp); perr == nil {
				fmt.Printf("console not reachable now (%v); the service keeps trying to enroll\n", err)
				return nil
			}
		}
		return err
	}
	// A replaced enrollment is set aside, so the machine can go back to its
	// previous console as the same agent (rename it to agent.json again).
	if _, err := os.Stat(*cfgPath); err == nil {
		kept := *cfgPath + ".replaced-" + time.Now().Format("20060102-150405")
		if os.Rename(*cfgPath, kept) == nil {
			fmt.Printf("previous enrollment kept as %s\n", kept)
		}
	}
	if err := cfg.Save(*cfgPath); err != nil {
		return fmt.Errorf("save configuration: %w", err)
	}
	fmt.Printf("enrolled as agent %s, configuration saved to %s\n", cfg.AgentUUID, *cfgPath)
	return nil
}

// program adapts the agent loop to the service manager.
type program struct {
	cfgPath string
	cancel  context.CancelFunc
	done    chan struct{}
}

// Start never fails because of a missing enrollment: an MSI installed
// without a token starts the service, which waits until "enroll" is run.
func (p *program) Start(s service.Service) error {
	logger := newLogger(p.cfgPath)
	if err := os.MkdirAll(filepath.Dir(p.cfgPath), 0o700); err == nil {
		if err := agent.SecureConfigDir(p.cfgPath); err != nil {
			logger.Warn("could not restrict access to the configuration directory", "err", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	go func() {
		defer close(p.done)
		var warned bool
		for {
			cfg, err := agent.LoadConfig(p.cfgPath)
			if err == nil {
				if p.cfgPath == agent.DefaultConfigPath() {
					agent.MarkEnrolled()
				}
				ag := agent.New(cfg, logger, version)
				go ag.ServeLocal(ctx)
				if !service.Interactive() {
					agent.StartTrayInSessions(logger)
				}
				ag.Run(ctx)
				return
			}
			if ok, perr := agent.TryPendingEnrollment(ctx, p.cfgPath, version); ok {
				logger.Info("enrolled with the code from the installer")
				continue
			} else if perr != nil {
				logger.Warn("enrollment not possible yet, retrying", "err", perr)
			}
			if !warned {
				logger.Warn("agent is not enrolled yet, waiting", "config", p.cfgPath, "err", err)
				warned = true
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(15 * time.Second):
			}
		}
	}()
	return nil
}

func (p *program) Stop(s service.Service) error {
	if p.cancel != nil {
		p.cancel()
		<-p.done
	}
	return nil
}

func newLogger(cfgPath string) *slog.Logger {
	var w io.Writer = os.Stdout
	os.MkdirAll(filepath.Dir(cfgPath), 0o700)
	logPath := filepath.Join(filepath.Dir(cfgPath), "agent.log")
	if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		// Interactive runs and systemd (journal) also get the log on stdout;
		// the Windows service only writes the file.
		if service.Interactive() || runtime.GOOS != "windows" {
			w = io.MultiWriter(os.Stdout, f)
		} else {
			w = f
		}
	}
	return slog.New(slog.NewTextHandler(w, nil))
}

func newService(cfgPath string) (service.Service, *program, error) {
	p := &program{cfgPath: cfgPath}
	args := []string{"service", "run"}
	if cfgPath != agent.DefaultConfigPath() {
		args = append(args, "--config", cfgPath)
	}
	svc, err := service.New(p, &service.Config{
		Name:        serviceName,
		DisplayName: "BackupZit Agent",
		Description: "Backs up this machine as instructed by the BackupZit management server.",
		Arguments:   args,
		Option: service.KeyValue{
			"OnFailure":              "restart",
			"OnFailureDelayDuration": "30s",
			"DelayedAutoStart":       true,
			"Restart":                "always",
		},
	})
	return svc, p, err
}

// cmdService handles: service install|uninstall|start|stop|status|run
func cmdService(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: backupzit-agent service install|uninstall|start|stop|status|run [--config FILE]")
	}
	action := args[0]
	fs := flag.NewFlagSet("service", flag.ExitOnError)
	cfgPath := fs.String("config", agent.DefaultConfigPath(), "agent configuration file")
	fs.Parse(args[1:])
	svc, _, err := newService(*cfgPath)
	if err != nil {
		return err
	}
	switch action {
	case "run":
		// Invoked by the service manager (or manually in the foreground).
		return svc.Run()
	case "status":
		st, err := svc.Status()
		if err != nil {
			return err
		}
		fmt.Println(map[service.Status]string{
			service.StatusRunning: "running", service.StatusStopped: "stopped", service.StatusUnknown: "unknown",
		}[st])
		return nil
	case "install":
		if _, err := agent.LoadConfig(*cfgPath); err != nil {
			return fmt.Errorf("enroll the agent before installing the service: %w", err)
		}
	}
	if err := service.Control(svc, action); err != nil {
		return err
	}
	fmt.Printf("service %s: %s ok\n", serviceName, action)
	return nil
}

// cmdSetServer points an enrolled agent at a new address of the console
// (after the console's IP address or name changed). The console's
// certificate must stay the same.
func cmdSetServer(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("set-server", flag.ExitOnError)
	cfgPath := fs.String("config", agent.DefaultConfigPath(), "agent configuration file")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: backupzit-agent set-server https://new-address:8443")
	}
	u, err := url.Parse(strings.TrimRight(fs.Arg(0), "/"))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("the address must look like https://backup.example.com:8443")
	}
	cfg, err := agent.LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := agent.ProbeServer(pctx, u.String(), cfg.Fingerprint); err != nil {
		return fmt.Errorf("the console does not answer at %s with its certificate %s: %w", u, cfg.Fingerprint, err)
	}
	old := cfg.ServerURL
	cfg.ServerURL = u.String()
	if err := cfg.Save(*cfgPath); err != nil {
		return err
	}
	fmt.Printf("console address changed from %s to %s; restart the agent service to use it\n", old, cfg.ServerURL)
	return nil
}

// cmdForget is run by the uninstaller: the enrollment is set aside (renamed,
// not deleted), so a new installation asks for an enrollment code again.
func cmdForget(args []string) error {
	fs := flag.NewFlagSet("forget", flag.ExitOnError)
	cfgPath := fs.String("config", agent.DefaultConfigPath(), "agent configuration file")
	fs.Parse(args)
	agent.UnmarkEnrolled()
	if _, err := os.Stat(*cfgPath); err != nil {
		return nil
	}
	kept := *cfgPath + ".removed-" + time.Now().Format("20060102-150405")
	if err := os.Rename(*cfgPath, kept); err != nil {
		return err
	}
	fmt.Println("enrollment set aside in", kept)
	return nil
}
