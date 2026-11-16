package server_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// fakeOIDC is a minimal OpenID Connect provider in the style of Authelia:
// it offers the "groups" scope and puts the groups only into the userinfo
// response, not into the ID token.
type fakeOIDC struct {
	ts     *httptest.Server
	key    *rsa.PrivateKey
	mu     sync.Mutex
	nonce  string
	groups []string
}

func newFakeOIDC(t *testing.T, groups ...string) *fakeOIDC {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeOIDC{key: key, groups: groups}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		u := f.ts.URL
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": u, "authorization_endpoint": u + "/authorize", "token_endpoint": u + "/token",
			"userinfo_endpoint": u + "/userinfo", "jwks_uri": u + "/jwks",
			"scopes_supported":                      []string{"openid", "profile", "email", "groups", "offline_access"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		nonce := f.nonce
		f.mu.Unlock()
		signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		claims, _ := json.Marshal(map[string]any{"iss": f.ts.URL, "sub": "u-123", "aud": "console", "exp": time.Now().Add(time.Hour).Unix(),
			"iat": time.Now().Unix(), "nonce": nonce, "preferred_username": "alice", "email": "alice@example.com"})
		jws, err := signer.Sign(claims)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		idt, _ := jws.CompactSerialize()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at-1", "token_type": "Bearer", "expires_in": 3600, "id_token": idt})
	})
	mux.HandleFunc("GET /userinfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer at-1" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"sub": "u-123", "groups": f.groups})
	})
	f.ts = httptest.NewTLSServer(mux)
	t.Cleanup(f.ts.Close)
	return f
}

func (f *fakeOIDC) caPEM() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.ts.Certificate().Raw}))
}

// TestOIDCGroupsFromUserInfo: the console requests the groups scope the
// provider offers and reads the groups from userinfo when the ID token has
// none (issue #30, Authelia).
func TestOIDCGroupsFromUserInfo(t *testing.T) {
	e := setup(t)
	idp := newFakeOIDC(t, "bz-admins", "staff")
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	_, loc, body := admin.do("POST", "/settings/sso", url.Values{
		"protocol": {"oidc"}, "issuer": {idp.ts.URL}, "client_id": {"console"}, "client_secret": {"s3cret"},
		"scopes": {"offline_access"}, "ca_cert": {idp.caPEM()}, "admin_group": {"bz-admins"}, "default_role": {""},
	})
	if !strings.HasPrefix(loc, "/settings/sso") {
		t.Fatalf("save SSO settings: %s %.300s", loc, body)
	}

	user := newClient(t, e)
	status, auth, _ := user.do("GET", "/login/sso", nil)
	if status != http.StatusSeeOther {
		t.Fatalf("start: %d", status)
	}
	au, err := url.Parse(auth)
	if err != nil {
		t.Fatal(err)
	}
	q := au.Query()
	scopes := strings.Fields(q.Get("scope"))
	for _, want := range []string{"openid", "profile", "email", "groups", "offline_access"} {
		found := false
		for _, s := range scopes {
			found = found || s == want
		}
		if !found {
			t.Errorf("scope %q not requested: %v", want, scopes)
		}
	}
	idp.mu.Lock()
	idp.nonce = q.Get("nonce")
	idp.mu.Unlock()

	_, loc, body = user.do("GET", "/login/sso/callback?code=c1&state="+url.QueryEscape(q.Get("state")), nil)
	if loc != "/" {
		t.Fatalf("callback did not sign in: %q %.400s", loc, body)
	}
	// alice got the administrator role from her userinfo groups.
	if status, _, _ := user.do("GET", "/settings/sso", nil); status != http.StatusOK {
		t.Errorf("alice is not an administrator: settings page %d", status)
	}
}

// Without a matching group and without a default role, sign-in is refused.
func TestOIDCNoGroupRefused(t *testing.T) {
	e := setup(t)
	idp := newFakeOIDC(t, "other")
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	admin.do("POST", "/settings/sso", url.Values{
		"protocol": {"oidc"}, "issuer": {idp.ts.URL}, "client_id": {"console"}, "client_secret": {"s3cret"},
		"ca_cert": {idp.caPEM()}, "admin_group": {"bz-admins"},
	})
	user := newClient(t, e)
	_, auth, _ := user.do("GET", "/login/sso", nil)
	au, _ := url.Parse(auth)
	idp.mu.Lock()
	idp.nonce = au.Query().Get("nonce")
	idp.mu.Unlock()
	status, loc, body := user.do("GET", "/login/sso/callback?code=c1&state="+url.QueryEscape(au.Query().Get("state")), nil)
	if status != http.StatusUnauthorized || loc == "/" || !strings.Contains(body, "not in any BackupZit group") {
		t.Fatalf("sign-in without a group: %d %q %.300s", status, loc, body)
	}
}
