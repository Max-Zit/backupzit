package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Who may reach the web console. Agents always reach their API (enrollment,
// polling, results, downloads, push installation), so the console can be
// published for agents while the web console stays limited to the
// administrators' addresses, and the agent port can serve agents only.

const settingWebAccess = "web_access"

// WebAccess limits the web console.
type WebAccess struct {
	// Allow lists the addresses or networks the web console answers;
	// empty: everyone.
	Allow []string `json:"allow,omitempty"`
	// AgentPortOnly: the main port (8443) serves agents only; the web
	// console is on the web port (Settings → Certificate).
	AgentPortOnly bool `json:"agent_port_only,omitempty"`
}

// Validate checks the networks.
func (a *WebAccess) Validate() error {
	for _, n := range a.Allow {
		if _, err := parsePrefix(n); err != nil {
			return fmt.Errorf("%q is not an IP address or network (e.g. 203.0.113.10 or 10.0.0.0/24)", n)
		}
	}
	return nil
}

// agentPath tells whether p belongs to the agents' API.
func agentPath(p string) bool {
	return strings.HasPrefix(p, "/api/agent/") || strings.HasPrefix(p, "/api/deploy/")
}

type webAccessCache struct {
	mu sync.Mutex
	at time.Time
	v  WebAccess
}

func (s *Server) webAccess(ctx context.Context) WebAccess {
	s.access.mu.Lock()
	defer s.access.mu.Unlock()
	if time.Since(s.access.at) < 15*time.Second {
		return s.access.v
	}
	var a WebAccess
	if s.store != nil {
		if err := s.store.GetSetting(ctx, settingWebAccess, &a); err != nil || a.Validate() != nil {
			a = WebAccess{}
		}
	}
	s.access.v, s.access.at = a, time.Now()
	return a
}

func (s *Server) reloadWebAccess() {
	s.access.mu.Lock()
	s.access.at = time.Time{}
	s.access.mu.Unlock()
}

// webPortActive tells whether the web port with the trusted certificate
// serves the console.
func (s *Server) webPortActive() bool {
	return s.web != nil && s.web.WebAddress() != ""
}

func loopback(ip string) bool {
	a, err := netip.ParseAddr(ip)
	return err == nil && a.Unmap().IsLoopback()
}

// limitAccess hides the web console from addresses that may not use it.
// main is the agents' port.
func (s *Server) limitAccess(h http.Handler, main bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if agentPath(r.URL.Path) {
			h.ServeHTTP(w, r)
			return
		}
		ip := s.guard.ClientIP(r)
		if !loopback(ip) {
			a := s.webAccess(r.Context())
			if (main && a.AgentPortOnly && s.webPortActive()) || (len(a.Allow) > 0 && !inPrefixes(ip, a.Allow)) {
				http.NotFound(w, r)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

func (s *Server) handleSettingsWebAccess(w http.ResponseWriter, r *http.Request, user string) {
	const back = "/settings/security#web-access"
	a := WebAccess{Allow: splitNetworks(r.FormValue("allow")), AgentPortOnly: r.FormValue("agent_port_only") == "on"}
	if err := a.Validate(); err != nil {
		redirectErr(w, r, back, err)
		return
	}
	ip := s.guard.ClientIP(r)
	if len(a.Allow) > 0 && !loopback(ip) && !inPrefixes(ip, a.Allow) {
		redirectErr(w, r, back, fmt.Errorf("your own address %s is not in the list; add it, or you would lock yourself out", ip))
		return
	}
	if a.AgentPortOnly && !s.webPortActive() {
		redirectErr(w, r, back, errors.New("the web console needs its own port first: set up a certificate under Settings → Certificate"))
		return
	}
	if a.AgentPortOnly && s.onMainPort(r) {
		redirectErr(w, r, back, fmt.Errorf("you are using the agents' port; open the console at %s and change this setting there", s.web.WebAddress()))
		return
	}
	if err := s.store.SetSetting(r.Context(), settingWebAccess, a); err != nil {
		s.serverError(w, err)
		return
	}
	s.reloadWebAccess()
	s.audit(r, "settings.web_access", "allowed %v, agent port for agents only %v", a.Allow, a.AgentPortOnly)
	redirectMsg(w, r, back, "Web console access saved.")
}

// onMainPort tells whether r arrived on the agents' port (not the web port).
func (s *Server) onMainPort(r *http.Request) bool {
	if s.web == nil {
		return true
	}
	addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return true
	}
	_, port, _ := net.SplitHostPort(addr.String())
	s.web.mu.Lock()
	webPort := s.web.cfg.Port
	s.web.mu.Unlock()
	return port != fmt.Sprint(webPort)
}

// ResetWebAccess lets everyone use the web console again on every port
// (the appliance's setup menu, for a lock-out).
func (s *Store) ResetWebAccess(ctx context.Context) error {
	return s.SetSetting(ctx, settingWebAccess, WebAccess{})
}
