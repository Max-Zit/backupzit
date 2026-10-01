package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/backupzit/backupzit/internal/agent"
	"github.com/backupzit/backupzit/internal/imaging"
)

// recoveryConfig is read from \backupzit\recovery.json on any drive (the
// boot USB/ISO), so recovery media can connect without typing.
type recoveryConfig struct {
	Server      string `json:"server"`
	Token       string `json:"token"`
	Fingerprint string `json:"fingerprint"`
}

func findRecoveryConfig() (recoveryConfig, string) {
	for d := 'C'; d <= 'Z'; d++ {
		p := string(d) + `:\backupzit\recovery.json`
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var c recoveryConfig
		if json.Unmarshal(b, &c) == nil && c.Server != "" {
			return c, p
		}
	}
	return recoveryConfig{}, ""
}

func prompt(in *bufio.Reader, q string) string {
	fmt.Print(q)
	s, _ := in.ReadString('\n')
	return strings.TrimSpace(s)
}

// cmdRecovery runs a temporary agent from recovery media (WinPE). It
// connects to the server, reports the local disks and executes restore
// runs until the machine is rebooted.
func cmdRecovery(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("recovery", flag.ExitOnError)
	c := recoveryConfig{}
	fs.StringVar(&c.Server, "server", "", "server URL")
	fs.StringVar(&c.Token, "token", "", "recovery/enrollment token")
	fs.StringVar(&c.Fingerprint, "fingerprint", "", "server certificate fingerprint")
	fs.Parse(args)

	fmt.Println("==============================================")
	fmt.Println(" BackupZit recovery environment  " + version)
	fmt.Println("==============================================")
	if c.Server == "" {
		if fc, path := findRecoveryConfig(); path != "" {
			fmt.Println("using settings from", path)
			c = fc
		}
	}
	in := bufio.NewReader(os.Stdin)
	for c.Server == "" {
		c.Server = prompt(in, "Server address (e.g. https://192.168.1.10:8443): ")
	}
	if !strings.HasPrefix(c.Server, "https://") {
		c.Server = "https://" + c.Server
	}
	for c.Token == "" {
		c.Token = prompt(in, "Recovery code (Console > Recovery): ")
	}
	if c.Fingerprint == "" {
		fp, err := agent.FetchFingerprint(ctx, c.Server)
		if err != nil {
			return fmt.Errorf("cannot reach %s: %w", c.Server, err)
		}
		fmt.Println("The server presented this certificate fingerprint:")
		fmt.Println("  " + fp)
		if a := prompt(in, "Does it match the fingerprint shown in the console? [y/N]: "); !strings.EqualFold(a, "y") {
			return errors.New("fingerprint not confirmed")
		}
		c.Fingerprint = fp
	}

	var cfg *agent.Config
	var err error
	for attempt := 1; ; attempt++ {
		cfg, err = agent.EnrollRecovery(ctx, c.Server, c.Token, c.Fingerprint, version)
		if err == nil {
			break
		}
		fmt.Printf("connecting to %s failed: %v\n", c.Server, err)
		if attempt >= 30 || strings.Contains(err.Error(), "token") || strings.Contains(err.Error(), "fingerprint") {
			return err
		}
		time.Sleep(5 * time.Second) // network may still be starting
	}
	host, _ := os.Hostname()
	fmt.Printf("connected to %s as %s\n\n", c.Server, host)
	if disks, err := imaging.ListDisks(); err == nil {
		fmt.Println("Local disks:")
		for _, d := range disks {
			fmt.Printf("  Disk %d: %s, %s, %d partitions\n", d.Number, d.Model, humanBytes(d.Size), len(d.Partitions))
		}
	}
	fmt.Println("\nWaiting for a restore started from the console. Keep this window open.")

	a := agent.New(cfg, slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})), version)
	a.VSS = false
	a.InventoryInterval = time.Minute
	a.Run(ctx)
	return nil
}
