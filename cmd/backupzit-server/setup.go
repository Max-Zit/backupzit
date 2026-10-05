package main

// The text menu on the appliance's screen ("backupzit-server --setup", run
// as root): network settings, server name, the web console's admin
// password, sign-in blocks, restart, reboot and shut down. It starts by
// itself when bzadmin signs in on the local screen.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"golang.org/x/term"

	"github.com/max-zit/backupzit/internal/netcfg"
	"github.com/max-zit/backupzit/internal/server"
)

const serverEnv = "/etc/backupzit/server.env"

type setupMenu struct {
	in  *bufio.Reader
	sys *netcfg.System
}

func runSetup(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return errors.New("the setup menu needs root: sudo backupzit-server --setup")
	}
	m := &setupMenu{in: bufio.NewReader(os.Stdin), sys: netcfg.Default}
	for {
		m.header(ctx)
		fmt.Print(`  1) Network (DHCP or static address, gateway, DNS)
  2) Server name
  3) SSH (on or off, port)
  4) Reset the password of the web console user "admin"
  5) Lift sign-in blocks
  6) Restart the web console
  7) Reboot the server
  8) Shut down the server
  0) Command line (open this menu again with: sudo backupzit-server --setup)

`)
		switch m.ask("Choose", "") {
		case "1":
			m.network(ctx)
		case "2":
			m.hostname(ctx)
		case "3":
			m.ssh(ctx)
		case "4":
			m.password(ctx)
		case "5":
			m.unblock(ctx)
		case "6":
			m.run(ctx, "Restarting the web console", "systemctl", "restart", "backupzit-server")
		case "7":
			if m.yes("Reboot the server now? Running backups are interrupted") {
				return exec.CommandContext(ctx, "systemctl", "reboot").Run()
			}
		case "8":
			if m.yes("Shut down the server now? Running backups are interrupted") {
				return exec.CommandContext(ctx, "systemctl", "poweroff").Run()
			}
		case "0", "q", "exit":
			return nil
		}
	}
}

func (m *setupMenu) header(ctx context.Context) {
	fmt.Print("\033[H\033[2J")
	host, _ := os.Hostname()
	fmt.Printf("\n  BackupZit appliance %s — %s\n  %s\n\n", version, host, strings.Repeat("─", 60))
	st, _ := m.sys.Current(ctx)
	if st != nil {
		mode := "DHCP"
		if !st.DHCP {
			mode = "static"
		}
		fmt.Printf("  Network:      %s (%s)  gateway %s  DNS %s\n", strings.Join(st.Addresses, ", "), mode, orDash(st.Gateway), orDash(strings.Join(st.DNS, ", ")))
		for _, a := range st.Addresses {
			ip, _, _ := strings.Cut(a, "/")
			fmt.Printf("  Web console:  https://%s:8443\n", ip)
		}
	}
	ssh := m.sys.SSH(ctx)
	if ssh.Enabled {
		fmt.Printf("  SSH:          on, port %d\n", ssh.Port)
	} else {
		fmt.Println("  SSH:          off")
	}
	state, _ := exec.CommandContext(ctx, "systemctl", "is-active", "backupzit-server").Output()
	fmt.Printf("  Service:      %s\n\n", strings.TrimSpace(string(state)))
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func (m *setupMenu) ask(prompt, def string) string {
	if def != "" {
		fmt.Printf("  %s [%s]: ", prompt, def)
	} else {
		fmt.Printf("  %s: ", prompt)
	}
	l, err := m.in.ReadString('\n')
	if err != nil && l == "" {
		return "exit" // end of input
	}
	if l = strings.TrimSpace(l); l == "" {
		return def
	}
	return l
}

func (m *setupMenu) yes(prompt string) bool {
	a := strings.ToLower(m.ask(prompt+" (y/N)", ""))
	return a == "y" || a == "yes"
}

func (m *setupMenu) pause() {
	fmt.Print("\n  Press Enter to continue.")
	m.in.ReadString('\n')
}

func (m *setupMenu) run(ctx context.Context, what string, name string, args ...string) {
	fmt.Printf("\n  %s…\n", what)
	if out, err := exec.CommandContext(ctx, name, args...).CombinedOutput(); err != nil {
		fmt.Printf("  Failed: %v %s\n", err, strings.TrimSpace(string(out)))
	} else {
		fmt.Println("  Done.")
	}
	m.pause()
}

func (m *setupMenu) network(ctx context.Context) {
	st, err := m.sys.Current(ctx)
	if err != nil {
		fmt.Println("  ", err)
		m.pause()
		return
	}
	c := netcfg.Config{}
	def := "s"
	if st.DHCP {
		def = "d"
	}
	if strings.HasPrefix(strings.ToLower(m.ask("DHCP or static address? (d/s)", def)), "d") {
		c.DHCP = true
	} else {
		addr := st.Address
		if addr == "" && len(st.Addresses) > 0 {
			addr = st.Addresses[0]
		}
		c.Address = m.ask("IPv4 address with prefix length", addr)
		c.Gateway = m.ask("Gateway (empty: none)", st.Gateway)
		c.DNS = strings.FieldsFunc(m.ask("DNS servers, separated by commas", strings.Join(st.DNS, ",")), func(r rune) bool { return r == ',' || r == ' ' })
	}
	if err := c.Validate(); err != nil {
		fmt.Println("\n  " + err.Error())
		m.pause()
		return
	}
	fmt.Println("\n  Agents keep the address they were enrolled with. If they reach this server")
	fmt.Println("  by its IP address, point them at the new one (or use a DNS name).")
	if !m.yes("Apply the new network settings") {
		return
	}
	if err := m.sys.Apply(ctx, c, false); err != nil {
		fmt.Println("\n  " + err.Error())
	} else {
		fmt.Println("\n  Applied.")
	}
	m.pause()
}

func (m *setupMenu) hostname(ctx context.Context) {
	st, _ := m.sys.Current(ctx)
	cur := ""
	if st != nil {
		cur = st.Hostname
	}
	c := netcfg.Config{DHCP: true, Hostname: m.ask("Server name", cur)}
	if c.Hostname == cur {
		return
	}
	if err := c.Validate(); err != nil {
		fmt.Println("\n  " + err.Error())
		m.pause()
		return
	}
	if err := m.sys.SetHostname(ctx, c.Hostname); err != nil {
		fmt.Println("\n  " + err.Error())
	} else {
		fmt.Println("\n  The server is now called " + c.Hostname + ".")
	}
	m.pause()
}

// store opens the console database with the settings of the service.
func (m *setupMenu) store(ctx context.Context) (*server.Store, func(), error) {
	env := map[string]string{}
	b, err := os.ReadFile(serverEnv)
	if err != nil {
		return nil, nil, err
	}
	for _, l := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok && !strings.HasPrefix(k, "#") {
			env[k] = strings.Trim(v, `"'`)
		}
	}
	if env["BACKUPZIT_DB"] == "" {
		return nil, nil, errors.New("BACKUPZIT_DB is not set in " + serverEnv)
	}
	pool, err := server.OpenDB(ctx, env["BACKUPZIT_DB"])
	if err != nil {
		return nil, nil, err
	}
	return server.NewStore(pool), pool.Close, nil
}

func (m *setupMenu) password(ctx context.Context) {
	fmt.Printf("\n  New password for \"admin\" (at least %d characters): ", server.MinPasswordLength)
	p1, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return
	}
	fmt.Print("  Repeat the password: ")
	p2, _ := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	switch {
	case string(p1) != string(p2):
		fmt.Println("\n  The passwords differ; nothing was changed.")
	case len([]rune(string(p1))) < server.MinPasswordLength:
		fmt.Println("\n  The password is too short; nothing was changed.")
	default:
		st, done, err := m.store(ctx)
		if err == nil {
			defer done()
			err = st.SetPassword(ctx, "admin", string(p1))
		}
		if err == nil {
			_, err = st.UnblockAddress(ctx, "all")
		}
		if err != nil {
			fmt.Println("\n  Failed: " + err.Error())
		} else {
			fmt.Println("\n  The password of \"admin\" was changed and sign-in blocks were lifted.")
		}
	}
	m.pause()
}

func (m *setupMenu) unblock(ctx context.Context) {
	st, done, err := m.store(ctx)
	if err == nil {
		defer done()
		var n int64
		if n, err = st.UnblockAddress(ctx, "all"); err == nil {
			fmt.Printf("\n  %d address(es) unblocked.\n", n)
		}
	}
	if err != nil {
		fmt.Println("\n  Failed: " + err.Error())
	}
	m.pause()
}

func (m *setupMenu) ssh(ctx context.Context) {
	cur := m.sys.SSH(ctx)
	def := "n"
	if cur.Enabled {
		def = "y"
	}
	c := netcfg.SSHConfig{Enabled: strings.HasPrefix(strings.ToLower(m.ask("SSH on? (y/n)", def)), "y"), Port: cur.Port}
	if c.Enabled {
		p, err := strconv.Atoi(m.ask("SSH port", strconv.Itoa(cur.Port)))
		if err != nil {
			fmt.Println("\n  The port must be a number.")
			m.pause()
			return
		}
		c.Port = p
	}
	if err := m.sys.ApplySSH(ctx, c); err != nil {
		fmt.Println("\n  " + err.Error())
	} else if c.Enabled {
		fmt.Printf("\n  SSH is on, port %d.\n", c.Port)
	} else {
		fmt.Println("\n  SSH is off. This menu on the server's screen still works.")
	}
	m.pause()
}
