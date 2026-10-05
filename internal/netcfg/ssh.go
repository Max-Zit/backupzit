package netcfg

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// SSHPortFile holds the SSH port BackupZit set (absent: port 22).
const SSHPortFile = "/etc/ssh/sshd_config.d/60-backupzit-port.conf"

// SSHConfig switches the appliance's SSH server on or off and sets its port.
type SSHConfig struct {
	Enabled bool `json:"enabled"`
	Port    int  `json:"port"`
}

// reservedPorts are used by BackupZit itself.
var reservedPorts = map[int]string{80: "the console (HTTP)", 443: "the console (HTTPS)", 8443: "the console and the agents", 5432: "PostgreSQL", 111: "instant recovery (NFS)", 2049: "instant recovery (NFS)"}

// Validate checks c.
func (c *SSHConfig) Validate() error {
	if c.Port == 0 {
		c.Port = 22
	}
	if c.Port < 1 || c.Port > 65535 {
		return errors.New("the SSH port must be between 1 and 65535")
	}
	if what, ok := reservedPorts[c.Port]; ok {
		return fmt.Errorf("port %d is used by %s", c.Port, what)
	}
	return nil
}

// sshUnit is the SSH service: "ssh" on Debian, "sshd" on RHEL systems.
func (s *System) sshUnit(ctx context.Context) string {
	if _, err := s.Run(ctx, "systemctl", "cat", "ssh.service"); err == nil {
		return "ssh"
	}
	return "sshd"
}

// SSH reads the current SSH settings.
func (s *System) SSH(ctx context.Context) SSHConfig {
	c := SSHConfig{Port: 22}
	if b, err := os.ReadFile(s.path(SSHPortFile)); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if f := strings.Fields(l); len(f) == 2 && f[0] == "Port" {
				c.Port, _ = strconv.Atoi(f[1])
			}
		}
	}
	_, err := s.Run(ctx, "systemctl", "is-active", "--quiet", s.sshUnit(ctx))
	c.Enabled = err == nil
	return c
}

// ApplySSH sets the port and starts or stops the SSH server. A port the SSH
// server refuses (sshd -t) leaves the previous setting in place.
func (s *System) ApplySSH(ctx context.Context, c SSHConfig) error {
	if err := c.Validate(); err != nil {
		return err
	}
	unit := s.sshUnit(ctx)
	old, oldErr := os.ReadFile(s.path(SSHPortFile))
	restore := func() {
		if oldErr == nil {
			os.WriteFile(s.path(SSHPortFile), old, 0o644)
		} else {
			os.Remove(s.path(SSHPortFile))
		}
	}
	if c.Port == 22 {
		os.Remove(s.path(SSHPortFile))
	} else {
		if err := os.WriteFile(s.path(SSHPortFile), []byte(fmt.Sprintf("# Set by BackupZit (appliance menu or Settings → Network).\nPort %d\n", c.Port)), 0o644); err != nil {
			return err
		}
	}
	if out, err := s.Run(ctx, "sshd", "-t"); err != nil {
		restore()
		return fmt.Errorf("the SSH server refused the setting, nothing was changed: %s", strings.TrimSpace(out+" "+err.Error()))
	}
	// Debian 13 can start SSH through ssh.socket, which listens on the port
	// itself; BackupZit runs the service directly so the port applies.
	s.Run(ctx, "systemctl", "disable", "--now", "ssh.socket")
	if !c.Enabled {
		_, err := s.Run(ctx, "systemctl", "disable", "--now", unit+".service")
		return err
	}
	if _, err := s.Run(ctx, "systemctl", "enable", unit+".service"); err != nil {
		return err
	}
	_, err := s.Run(ctx, "systemctl", "restart", unit+".service")
	return err
}
