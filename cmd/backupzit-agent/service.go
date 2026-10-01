package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/backupzit/backupzit/internal/agent"
	"github.com/kardianos/service"
)

const serviceName = "backupzit-agent"

func cmdEnroll(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	server := fs.String("server", "", "management server URL, e.g. https://backup.example.com:8443")
	token := fs.String("token", "", "enrollment token from the console")
	fp := fs.String("fingerprint", "", "server certificate fingerprint SHA256:... from the console")
	cfgPath := fs.String("config", agent.DefaultConfigPath(), "agent configuration file")
	force := fs.Bool("force", false, "replace an existing enrollment")
	fs.Parse(args)
	if *server == "" || *token == "" || *fp == "" {
		fs.Usage()
		return errors.New("--server, --token and --fingerprint are required")
	}
	if _, err := os.Stat(*cfgPath); err == nil && !*force {
		return fmt.Errorf("agent is already enrolled (%s); use --force to enroll again", *cfgPath)
	}
	cfg, err := agent.Enroll(ctx, *server, *token, *fp, version)
	if err != nil {
		return err
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

func (p *program) Start(s service.Service) error {
	cfg, err := agent.LoadConfig(p.cfgPath)
	if err != nil {
		return err
	}
	logger := newLogger(p.cfgPath)
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	go func() {
		defer close(p.done)
		agent.New(cfg, logger, version).Run(ctx)
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
	logPath := filepath.Join(filepath.Dir(cfgPath), "agent.log")
	if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		if service.Interactive() {
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
		DisplayName: "backupzit Agent",
		Description: "Backs up this machine as instructed by the backupzit management server.",
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
