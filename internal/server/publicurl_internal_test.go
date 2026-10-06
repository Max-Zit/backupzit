package server

import (
	"net/http/httptest"
	"testing"
)

// The enrollment code names the agents' port even when the page is open on
// the web port (whose certificate the agents do not pin).
func TestPublicURLAgentPort(t *testing.T) {
	s := &Server{AgentPort: "8443"}
	for host, want := range map[string]string{
		"192.168.1.5":     "https://192.168.1.5:8443",
		"192.168.1.5:443": "https://192.168.1.5:8443",
		"backup.lan:8443": "https://backup.lan:8443",
		"[fd00::5]:443":   "https://[fd00::5]:8443",
	} {
		r := httptest.NewRequest("GET", "/agents", nil)
		r.Host = host
		if got := s.publicURL(r); got != want {
			t.Errorf("%s: %s, want %s", host, got, want)
		}
	}
	s.PublicURL = "https://backup.example.com:9443"
	if got := s.publicURL(httptest.NewRequest("GET", "/", nil)); got != s.PublicURL {
		t.Errorf("PublicURL ignored: %s", got)
	}
}
