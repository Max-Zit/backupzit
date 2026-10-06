package server

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coreos/go-oidc/v3/oidc"
)

// An identity provider with a self-signed certificate works once its
// certificate is entered as the CA.
func TestSSOCACert(t *testing.T) {
	var issuer string
	idp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/auth",
			"token_endpoint": issuer + "/token", "jwks_uri": issuer + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"}})
	}))
	defer idp.Close()
	issuer = idp.URL
	c := SSOSettings{Protocol: "oidc", Issuer: issuer, ClientID: "x", ClientSecret: "y", AdminGroup: "admins"}
	if _, err := oidc.NewProvider(oidc.ClientContext(context.Background(), c.httpClient()), issuer); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("without the CA: %v", err)
	}
	c.CACert = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: idp.Certificate().Raw}))
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := oidc.NewProvider(oidc.ClientContext(context.Background(), c.httpClient()), issuer); err != nil {
		t.Fatalf("with the CA: %v", err)
	}
	c.CACert = "not a certificate"
	if err := c.Validate(); err == nil {
		t.Fatal("invalid PEM accepted")
	}
}
