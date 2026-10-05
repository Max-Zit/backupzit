// Package netcfg reads and changes the network settings of the BackupZit
// appliance: DHCP or a static address, gateway, DNS servers and the server
// name. The appliance uses systemd-networkd; installations from the ISO
// that still use ifupdown are switched to systemd-networkd on the first
// change. Both the text menu on the appliance's screen and the console's
// root helper use this package, so they behave the same.
package netcfg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// NetworkFile is the systemd-networkd file BackupZit manages.
const NetworkFile = "/etc/systemd/network/50-dhcp.network"

// ApplianceMarker exists on the BackupZit appliance (written by its first
// start). Network settings are only offered there: on other servers they
// belong to the administrator of that server.
const ApplianceMarker = "/var/lib/backupzit/.appliance-ready"

// Config is the wanted network configuration.
type Config struct {
	DHCP     bool     `json:"dhcp"`
	Address  string   `json:"address,omitempty"` // with prefix, e.g. 192.168.1.20/24
	Gateway  string   `json:"gateway,omitempty"`
	DNS      []string `json:"dns,omitempty"`
	Hostname string   `json:"hostname,omitempty"`
}

// Status is the current state.
type Status struct {
	Config
	Interface string   `json:"interface"`
	Addresses []string `json:"addresses"` // current IPv4 addresses with prefix
	Managed   bool     `json:"managed"`   // NetworkFile exists
}

// System runs commands and maps paths (tests use a temporary root and a
// fake runner).
type System struct {
	Root string
	// GatewayWait is how long Apply waits for the gateway (default 30 s).
	GatewayWait time.Duration
	Run         func(ctx context.Context, name string, args ...string) (string, error)
}

// Default is the real system.
var Default = &System{Run: func(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}}

func (s *System) path(p string) string { return filepath.Join(s.Root, p) }

// IsAppliance tells whether this server is the BackupZit appliance.
func (s *System) IsAppliance() bool {
	_, err := os.Stat(s.path(ApplianceMarker))
	return err == nil
}

var hostnameRe = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

// Validate checks c and normalises its addresses.
func (c *Config) Validate() error {
	if c.Hostname != "" && !hostnameRe.MatchString(c.Hostname) {
		return errors.New("the server name may contain letters, digits and - (up to 63 characters, no dots)")
	}
	var dns []string
	for _, d := range c.DNS {
		if d = strings.TrimSpace(d); d == "" {
			continue
		}
		a, err := netip.ParseAddr(d)
		if err != nil {
			return fmt.Errorf("DNS server %q is not an IP address", d)
		}
		dns = append(dns, a.String())
	}
	if len(dns) > 3 {
		return errors.New("enter up to three DNS servers")
	}
	c.DNS = dns
	if c.DHCP {
		c.Address, c.Gateway = "", ""
		return nil
	}
	p, err := netip.ParsePrefix(strings.TrimSpace(c.Address))
	if err != nil || !p.Addr().Is4() {
		return errors.New("enter the IPv4 address with its prefix length, e.g. 192.168.1.20/24")
	}
	if p.Bits() < 8 || p.Bits() > 30 {
		return errors.New("the prefix length must be between /8 and /30")
	}
	if p.Addr() == p.Masked().Addr() {
		return errors.New("the address is the network address of its subnet")
	}
	c.Address = p.String()
	if strings.TrimSpace(c.Gateway) != "" {
		g, err := netip.ParseAddr(strings.TrimSpace(c.Gateway))
		if err != nil || !g.Is4() {
			return errors.New("the gateway is not an IPv4 address")
		}
		if !p.Masked().Contains(g) || g == p.Addr() {
			return fmt.Errorf("the gateway %s is not another address in %s", g, p.Masked())
		}
		c.Gateway = g.String()
	}
	if len(c.DNS) == 0 {
		return errors.New("enter at least one DNS server")
	}
	return nil
}

// networkFile renders the systemd-networkd file for c.
func networkFile(c Config) string {
	var b strings.Builder
	b.WriteString("# Managed by BackupZit (appliance menu or Settings → Network).\n[Match]\nName=en* eth*\n\n[Network]\n")
	if c.DHCP {
		b.WriteString("DHCP=yes\n")
	} else {
		fmt.Fprintf(&b, "Address=%s\n", c.Address)
		if c.Gateway != "" {
			fmt.Fprintf(&b, "Gateway=%s\n", c.Gateway)
		}
	}
	for _, d := range c.DNS {
		fmt.Fprintf(&b, "DNS=%s\n", d)
	}
	return b.String()
}

// parseNetworkFile reads the settings from a systemd-networkd file.
func parseNetworkFile(text string) Config {
	var c Config
	for _, l := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(l), "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "DHCP":
			c.DHCP = v == "yes" || v == "ipv4" || v == "true"
		case "Address":
			c.Address = v
		case "Gateway":
			c.Gateway = v
		case "DNS":
			c.DNS = append(c.DNS, strings.Fields(v)...)
		}
	}
	return c
}

// Current reads the configured and the live settings.
func (s *System) Current(ctx context.Context) (*Status, error) {
	st := &Status{}
	if b, err := os.ReadFile(s.path(NetworkFile)); err == nil {
		st.Config = parseNetworkFile(string(b))
		st.Managed = true
	} else {
		st.DHCP = true // ifupdown installations from the ISO
	}
	if b, err := os.ReadFile(s.path("/etc/hostname")); err == nil {
		st.Hostname = strings.TrimSpace(string(b))
	}
	if len(st.DNS) == 0 {
		st.DNS = resolvers(s.path("/etc/resolv.conf"))
	}
	if out, err := s.Run(ctx, "ip", "-j", "-4", "addr", "show", "scope", "global"); err == nil {
		var links []struct {
			Name  string `json:"ifname"`
			Addrs []struct {
				Local  string `json:"local"`
				Prefix int    `json:"prefixlen"`
			} `json:"addr_info"`
		}
		if json.Unmarshal([]byte(out), &links) == nil {
			for _, l := range links {
				for _, a := range l.Addrs {
					if st.Interface == "" {
						st.Interface = l.Name
					}
					st.Addresses = append(st.Addresses, fmt.Sprintf("%s/%d", a.Local, a.Prefix))
				}
			}
		}
	}
	if st.Gateway == "" {
		if out, err := s.Run(ctx, "ip", "-j", "-4", "route", "show", "default"); err == nil {
			var routes []struct {
				Gateway string `json:"gateway"`
			}
			if json.Unmarshal([]byte(out), &routes) == nil && len(routes) > 0 {
				st.Gateway = routes[0].Gateway
			}
		}
	}
	return st, nil
}

func resolvers(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) == 2 && f[0] == "nameserver" {
			out = append(out, f[1])
		}
	}
	return out
}

// Apply writes c and activates it. With check, a static configuration whose
// gateway does not answer within half a minute is undone again (so a typo
// in the web console cannot cut the appliance off the network); the text
// menu on the appliance's own screen applies without that check.
func (s *System) Apply(ctx context.Context, c Config, check bool) error {
	if err := c.Validate(); err != nil {
		return err
	}
	old, oldErr := os.ReadFile(s.path(NetworkFile))
	oldResolv, _ := os.ReadFile(s.path("/etc/resolv.conf"))
	if err := os.MkdirAll(filepath.Dir(s.path(NetworkFile)), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(s.path(NetworkFile), []byte(networkFile(c)), 0o644); err != nil {
		return err
	}
	if !c.DHCP {
		s.writeResolv(c.DNS)
	}
	if c.Hostname != "" {
		if err := s.SetHostname(ctx, c.Hostname); err != nil {
			return err
		}
	}
	if err := s.activate(ctx); err != nil {
		return err
	}
	if !check || c.DHCP || c.Gateway == "" {
		return nil
	}
	wait := s.GatewayWait
	if wait == 0 {
		wait = 30 * time.Second
	}
	if s.reachable(ctx, c.Gateway, wait) {
		return nil
	}
	// Undo: the previous file (or DHCP), the previous resolvers.
	if oldErr == nil {
		os.WriteFile(s.path(NetworkFile), old, 0o644)
	} else {
		os.WriteFile(s.path(NetworkFile), []byte(networkFile(Config{DHCP: true})), 0o644)
	}
	if oldResolv != nil {
		os.WriteFile(s.path("/etc/resolv.conf"), oldResolv, 0o644)
	}
	s.activate(ctx)
	return fmt.Errorf("the gateway %s did not answer with the new settings; the previous network settings were restored", c.Gateway)
}

// writeResolv sets the DNS servers, unless systemd-resolved manages
// /etc/resolv.conf (it then takes them from the network file).
func (s *System) writeResolv(dns []string) {
	if t, err := os.Readlink(s.path("/etc/resolv.conf")); err == nil && strings.Contains(t, "systemd") {
		return
	}
	var b strings.Builder
	b.WriteString("# Written by BackupZit (appliance network settings).\n")
	for _, d := range dns {
		fmt.Fprintf(&b, "nameserver %s\n", d)
	}
	os.WriteFile(s.path("/etc/resolv.conf"), []byte(b.String()), 0o644)
}

// SetHostname renames the server.
func (s *System) SetHostname(ctx context.Context, name string) error {
	if err := os.WriteFile(s.path("/etc/hostname"), []byte(name+"\n"), 0o644); err != nil {
		return err
	}
	hosts, _ := os.ReadFile(s.path("/etc/hosts"))
	var out []string
	found := false
	for _, l := range strings.Split(strings.TrimRight(string(hosts), "\n"), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "127.0.1.1") {
			l, found = "127.0.1.1\t"+name, true
		}
		out = append(out, l)
	}
	if !found {
		out = append(out, "127.0.1.1\t"+name)
	}
	if err := os.WriteFile(s.path("/etc/hosts"), []byte(strings.Join(out, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	_, err := s.Run(ctx, "hostname", name)
	return err
}

// activate makes systemd-networkd manage the network with the new file.
// Installations from the ISO used ifupdown: it is switched off for good.
func (s *System) activate(ctx context.Context) error {
	if b, err := os.ReadFile(s.path("/etc/network/interfaces")); err == nil && strings.Contains(string(b), "iface e") {
		os.WriteFile(s.path("/etc/network/interfaces"), []byte("auto lo\niface lo inet loopback\n"), 0o644)
		s.Run(ctx, "systemctl", "disable", "networking.service")
	}
	s.Run(ctx, "systemctl", "enable", "systemd-networkd.service")
	// Old addresses go away with the restart only if they are flushed.
	if st, err := s.Current(ctx); err == nil && st.Interface != "" {
		s.Run(ctx, "ip", "-4", "addr", "flush", "dev", st.Interface, "scope", "global")
	}
	_, err := s.Run(ctx, "systemctl", "restart", "systemd-networkd.service")
	return err
}

// reachable waits until the gateway answers a ping.
func (s *System) reachable(ctx context.Context, gw string, wait time.Duration) bool {
	if _, err := s.Run(ctx, "sh", "-c", "command -v ping"); err != nil {
		return true // no ping: cannot tell, keep the settings
	}
	end := time.Now().Add(wait)
	for time.Now().Before(end) {
		if _, err := s.Run(ctx, "ping", "-c", "1", "-W", "2", gw); err == nil {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(2 * time.Second):
		}
	}
	return false
}
