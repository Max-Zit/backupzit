package agent

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/tlsutil"
)

// A code made on a console's web port (another certificate) still
// enrolls: the agent tries the agents' port with the pinned certificate.
func TestEnrollFallsBackToAgentPort(t *testing.T) {
	serve := func(dir string, enroll bool) (*httptest.Server, string) {
		cert, err := tlsutil.LoadOrCreateCert(t.TempDir()+dir, []string{"127.0.0.1"})
		if err != nil {
			t.Fatal(err)
		}
		s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !enroll {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(api.EnrollResponse{AgentUUID: "u1", Secret: "s1"})
		}))
		s.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
		s.StartTLS()
		return s, tlsutil.CertFingerprint(cert)
	}
	agentSrv, fp := serve("/a", true)
	defer agentSrv.Close()
	webSrv, _ := serve("/w", false)
	defer webSrv.Close()
	_, port, _ := net.SplitHostPort(agentSrv.Listener.Addr().String())
	old := defaultAgentPort
	defaultAgentPort = port
	defer func() { defaultAgentPort = old }()

	cfg, err := Enroll(context.Background(), webSrv.URL, "tok", fp, "test")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServerURL != agentSrv.URL || cfg.AgentUUID != "u1" {
		t.Fatalf("enrolled with %s (%s), want %s", cfg.ServerURL, cfg.AgentUUID, agentSrv.URL)
	}
	// A wrong fingerprint is still refused on both ports.
	if _, err := Enroll(context.Background(), webSrv.URL, "tok", "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "test"); err == nil {
		t.Fatal("wrong fingerprint accepted")
	}
}
