package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/netcfg"
	"github.com/max-zit/backupzit/internal/update"
)

// Settings → Network: the appliance's address, server name and SSH,
// applied by the root helper (the console itself runs unprivileged).

// netSystem is the system whose network the page shows (tests replace it).
var netSystem = netcfg.Default

type networkPage struct {
	Available bool   // the appliance with the root helper
	Reason    string // why not
	Status    *netcfg.Status
	SSH       netcfg.SSHConfig
	TimeZone  string   // system time zone, e.g. Europe/Belgrade
	Zones     []string // the zones the system knows
	Result    *update.OSResult
	Pending   bool
	DNS       string
	Address   string // for the form: the static address or the current one
}

func (s *Server) networkPageData(ctx context.Context) networkPage {
	var p networkPage
	switch {
	case !netSystem.IsAppliance():
		p.Reason = "Network settings are managed here only on the BackupZit appliance. On this server, change them with the tools of its operating system."
		return p
	case s.upd == nil || !s.upd.helper:
		p.Reason = "The root helper of the console package is missing, so the console cannot change network settings."
		return p
	}
	p.Available = true
	p.Status, _ = netSystem.Current(ctx)
	if p.Status != nil {
		p.DNS = strings.Join(p.Status.DNS, ", ")
		p.Address = p.Status.Address
		if p.Address == "" && len(p.Status.Addresses) > 0 {
			p.Address = p.Status.Addresses[0]
		}
	}
	p.SSH = netSystem.SSH(ctx)
	p.TimeZone = netSystem.TimeZone()
	if out, err := netSystem.Run(ctx, "timedatectl", "list-timezones"); err == nil {
		p.Zones = strings.Fields(out)
	}
	var res update.OSResult
	if update.ReadJSON(filepath.Join(s.upd.dir, "net-result.json"), &res) == nil {
		p.Result = &res
	}
	if _, err := os.Stat(filepath.Join(s.upd.dir, "request.json")); err == nil {
		p.Pending = true
	}
	return p
}

// requestHelper hands a request to the root helper.
func (s *Server) requestHelper(req update.Request) error {
	if s.upd == nil || !s.upd.helper || !netSystem.IsAppliance() {
		return errors.New("network settings can only be changed on the BackupZit appliance")
	}
	if _, err := os.Stat(filepath.Join(s.upd.dir, "request.json")); err == nil {
		return errors.New("another task of the server is in progress; try again in a minute")
	}
	req.Requested = time.Now().UTC()
	return update.WriteJSON(filepath.Join(s.upd.dir, "request.json"), req, 0o640)
}

func (s *Server) handleNetworkSettings(w http.ResponseWriter, r *http.Request, user string) {
	const back = "/settings/network"
	c := netcfg.Config{DHCP: r.FormValue("mode") == "dhcp", Address: r.FormValue("address"), Gateway: r.FormValue("gateway"),
		DNS: strings.FieldsFunc(r.FormValue("dns"), func(r rune) bool { return r == ',' || r == ' ' }), Hostname: strings.TrimSpace(r.FormValue("hostname"))}
	if err := c.Validate(); err != nil {
		redirectErr(w, r, back, err)
		return
	}
	if err := s.requestHelper(update.Request{Action: update.ActionNetwork, Network: &c, User: user}); err != nil {
		redirectErr(w, r, back, err)
		return
	}
	s.audit(r, "settings.network", "%s", describeNetwork(c))
	msg := "Applying the network settings. If the address changes, open the console at the new address."
	if !c.DHCP {
		msg = "Applying the network settings: the console moves to the new address in a few seconds. If the gateway does not answer, the previous settings come back after half a minute."
	}
	redirectMsg(w, r, back, msg)
}

func describeNetwork(c netcfg.Config) string {
	d := "DHCP"
	if !c.DHCP {
		d = c.Address + " gateway " + c.Gateway + " DNS " + strings.Join(c.DNS, ",")
	}
	if c.Hostname != "" {
		d += ", name " + c.Hostname
	}
	return d
}

func (s *Server) handleSSHSettings(w http.ResponseWriter, r *http.Request, user string) {
	const back = "/settings/network#ssh"
	port, err := strconv.Atoi(strings.TrimSpace(r.FormValue("port")))
	if err != nil {
		redirectErr(w, r, back, errors.New("the SSH port must be a number"))
		return
	}
	c := netcfg.SSHConfig{Enabled: r.FormValue("enabled") == "on", Port: port}
	if err := c.Validate(); err != nil {
		redirectErr(w, r, back, err)
		return
	}
	if err := s.requestHelper(update.Request{Action: update.ActionSSH, SSH: &c, User: user}); err != nil {
		redirectErr(w, r, back, err)
		return
	}
	state := "off"
	if c.Enabled {
		state = "on, port " + strconv.Itoa(c.Port)
	}
	s.audit(r, "settings.ssh", "SSH %s", state)
	redirectMsg(w, r, back, "Applying the SSH settings; reload the page in a few seconds.")
}

func (s *Server) handleTimeZoneSettings(w http.ResponseWriter, r *http.Request, user string) {
	const back = "/settings/network#timezone"
	zone := strings.TrimSpace(r.FormValue("zone"))
	if err := netcfg.ValidTimeZone(zone); err != nil {
		redirectErr(w, r, back, err)
		return
	}
	if err := s.requestHelper(update.Request{Action: update.ActionTimeZone, TimeZone: zone, User: user}); err != nil {
		redirectErr(w, r, back, err)
		return
	}
	s.audit(r, "settings.timezone", "time zone %s", zone)
	redirectMsg(w, r, back, "Setting the time zone; the console restarts and is back in a few seconds.")
}
