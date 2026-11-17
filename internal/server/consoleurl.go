package server

import (
	"context"
	"net/http"
	"sync"
)

// Links in emails, chat messages and reports point to the console. Without
// --public-url (the appliance default) the console remembers the address
// its users open it with.

const settingConsoleURL = "console_url"

type seenURL struct {
	mu  sync.Mutex
	url string
}

// ConsoleURL is the console's address for links: the web port with the
// trusted certificate when it is on, else PublicURL, else the address a
// browser last used ("" if none is known). PublicURL is the address agents
// use (often port 8443); with "Port 8443 serves agents only" the web
// console does not answer there (issue #37).
func (s *Server) ConsoleURL() string {
	if s.web != nil {
		if u := s.web.WebAddress(); u != "" {
			return u
		}
	}
	if s.PublicURL != "" {
		return s.PublicURL
	}
	s.seen.mu.Lock()
	defer s.seen.mu.Unlock()
	if s.seen.url == "" && s.store != nil {
		_ = s.store.GetSetting(context.Background(), settingConsoleURL, &s.seen.url)
	}
	return s.seen.url
}

// noteConsoleURL remembers the address a signed-in user opened a page with.
func (s *Server) noteConsoleURL(r *http.Request) {
	if s.PublicURL != "" || r.Host == "" {
		return
	}
	scheme := "https"
	if r.TLS == nil {
		scheme = "http" // --dev-http
	}
	u := scheme + "://" + r.Host
	if s.ConsoleURL() == u {
		return
	}
	s.seen.mu.Lock()
	s.seen.url = u
	s.seen.mu.Unlock()
	if err := s.store.SetSetting(context.WithoutCancel(r.Context()), settingConsoleURL, u); err != nil {
		s.log.Error("remember the console address", "err", err)
	}
}
