package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/crewjam/saml"
	"golang.org/x/oauth2"
)

// Single sign-on with an identity provider: OpenID Connect (Microsoft
// Entra ID, Google, Okta, Keycloak…) or SAML 2.0 (ADFS, Entra ID, Okta…).
// Users are created at their first sign-in with the source "sso"; their
// role follows their groups at every sign-in, like LDAP users. Local
// accounts keep working, so an administrator can always sign in.

const settingSSO = "sso"

// SSOSettings configure single sign-on.
type SSOSettings struct {
	// Protocol is "" (off), "oidc" or "saml".
	Protocol string `json:"protocol"`
	// Label is the text of the sign-in button.
	Label string `json:"label,omitempty"`
	// OpenID Connect.
	Issuer       string `json:"issuer,omitempty"`
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	// SAML: the identity provider's metadata, by URL or pasted.
	MetadataURL string `json:"metadata_url,omitempty"`
	MetadataXML string `json:"metadata_xml,omitempty"`
	// CACert (PEM) is trusted for the identity provider's HTTPS, for one
	// with an internal CA or a self-signed certificate.
	CACert string `json:"ca_cert,omitempty"`
	// The console's SAML key and certificate (created once).
	SPKey  string `json:"sp_key,omitempty"`
	SPCert string `json:"sp_cert,omitempty"`
	// Claims or attributes: the user name and the groups.
	UsernameClaim string `json:"username_claim,omitempty"`
	GroupsClaim   string `json:"groups_claim,omitempty"`
	// Group names (or IDs) per role, highest role wins; DefaultRole for
	// users in none of them ("" = they cannot sign in).
	AdminGroup    string `json:"admin_group,omitempty"`
	OperatorGroup string `json:"operator_group,omitempty"`
	RestoreGroup  string `json:"restore_group,omitempty"`
	ViewerGroup   string `json:"viewer_group,omitempty"`
	DefaultRole   string `json:"default_role,omitempty"`
}

func (c SSOSettings) label() string {
	if c.Label != "" {
		return c.Label
	}
	return "Single sign-on"
}

func (c *SSOSettings) Validate() error {
	if c.CACert != "" && !x509.NewCertPool().AppendCertsFromPEM([]byte(c.CACert)) {
		return errors.New("the CA certificate must be in PEM format (-----BEGIN CERTIFICATE-----)")
	}
	switch c.Protocol {
	case "":
		return nil
	case "oidc":
		u, err := url.Parse(c.Issuer)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return errors.New("the issuer must be an https:// address, e.g. https://login.microsoftonline.com/<tenant>/v2.0")
		}
		if c.ClientID == "" || c.ClientSecret == "" {
			return errors.New("enter the client ID and the client secret of the application registered at the identity provider")
		}
	case "saml":
		if c.MetadataURL == "" && c.MetadataXML == "" {
			return errors.New("enter the identity provider's metadata URL or paste its metadata XML")
		}
		if c.MetadataURL != "" {
			if u, err := url.Parse(c.MetadataURL); err != nil || u.Scheme != "https" || u.Host == "" {
				return errors.New("the metadata URL must be an https:// address")
			}
		}
	default:
		return errors.New("unknown single sign-on protocol")
	}
	switch c.DefaultRole {
	case "", "admin", "operator", "restore", "viewer":
	default:
		return errors.New("unknown default role")
	}
	if c.AdminGroup == "" && c.OperatorGroup == "" && c.RestoreGroup == "" && c.ViewerGroup == "" && c.DefaultRole == "" {
		return errors.New("map at least one group to a role, or choose a role for all users of the identity provider")
	}
	return nil
}

// role maps the user's groups to a role.
func (c SSOSettings) role(groups []string) (string, error) {
	in := func(g string) bool {
		if g == "" {
			return false
		}
		for _, x := range groups {
			if strings.EqualFold(strings.TrimSpace(x), strings.TrimSpace(g)) {
				return true
			}
		}
		return false
	}
	switch {
	case in(c.AdminGroup):
		return "admin", nil
	case in(c.OperatorGroup):
		return "operator", nil
	case in(c.RestoreGroup):
		return "restore", nil
	case in(c.ViewerGroup):
		return "viewer", nil
	case c.DefaultRole != "":
		return c.DefaultRole, nil
	}
	return "", errNoRole
}

func (s *Server) ssoSettings(ctx context.Context) SSOSettings {
	var c SSOSettings
	if s.store == nil {
		return c
	}
	s.store.GetSetting(ctx, settingSSO, &c)
	return c
}

// baseURL is the console's address as the browser uses it (for redirect
// and SAML addresses, which the identity provider must know).
func (s *Server) baseURL(r *http.Request) string {
	scheme := "http"
	if s.guard.Secure(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// ---- state cookie

const ssoCookie = "bz_sso"

// ssoState signs what the callback must find again (state, nonce, PKCE
// verifier or SAML request ID) for 10 minutes.
func ssoState(values ...string) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(strings.Join(append(values, strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10)), "\n")))
	m := hmac.New(sha256.New, flashKey)
	m.Write([]byte("sso:" + payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func parseSSOState(tok string) ([]string, bool) {
	payload, sig, ok := strings.Cut(tok, ".")
	if !ok {
		return nil, false
	}
	m := hmac.New(sha256.New, flashKey)
	m.Write([]byte("sso:" + payload))
	if !hmac.Equal([]byte(sig), []byte(base64.RawURLEncoding.EncodeToString(m.Sum(nil)))) {
		return nil, false
	}
	b, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return nil, false
	}
	v := strings.Split(string(b), "\n")
	exp, err := strconv.ParseInt(v[len(v)-1], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return nil, false
	}
	return v[:len(v)-1], true
}

func (s *Server) setSSOCookie(w http.ResponseWriter, r *http.Request, v string) {
	// The SAML answer arrives as a POST from the identity provider's site:
	// browsers send the cookie then only with SameSite=None, which needs
	// HTTPS. Without HTTPS (development) Lax still works for OpenID Connect.
	ss := http.SameSiteLaxMode
	if s.guard.Secure(r) {
		ss = http.SameSiteNoneMode
	}
	http.SetCookie(w, &http.Cookie{Name: ssoCookie, Value: v, Path: "/login/sso", MaxAge: 600, HttpOnly: true,
		Secure: s.guard.Secure(r), SameSite: ss})
}

func randomString() string {
	b := make([]byte, 24)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// ---- providers (cached per configuration)

type ssoCache struct {
	mu       sync.Mutex
	key      string
	provider *oidc.Provider
	idp      *saml.EntityDescriptor
}

var ssoProviders ssoCache

func (c SSOSettings) cacheKey() string {
	return c.Protocol + "\n" + c.Issuer + "\n" + c.MetadataURL + "\n" + c.MetadataXML
}

func (s *Server) oidcConfig(ctx context.Context, c SSOSettings, r *http.Request) (*oauth2.Config, *oidc.IDTokenVerifier, error) {
	ssoProviders.mu.Lock()
	defer ssoProviders.mu.Unlock()
	if ssoProviders.key != c.cacheKey() || ssoProviders.provider == nil {
		pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		p, err := oidc.NewProvider(oidc.ClientContext(pctx, c.httpClient()), strings.TrimRight(c.Issuer, "/"))
		if err != nil {
			return nil, nil, fmt.Errorf("identity provider %s: %w", c.Issuer, err)
		}
		ssoProviders.key, ssoProviders.provider = c.cacheKey(), p
	}
	p := ssoProviders.provider
	scopes := []string{oidc.ScopeOpenID, "profile", "email"}
	return &oauth2.Config{ClientID: c.ClientID, ClientSecret: c.ClientSecret, Endpoint: p.Endpoint(),
			RedirectURL: s.baseURL(r) + "/login/sso/callback", Scopes: scopes},
		p.Verifier(&oidc.Config{ClientID: c.ClientID}), nil
}

// serviceProvider is the console as a SAML service provider.
func (s *Server) serviceProvider(ctx context.Context, c SSOSettings, r *http.Request) (*saml.ServiceProvider, error) {
	ssoProviders.mu.Lock()
	idp := ssoProviders.idp
	if ssoProviders.key != c.cacheKey() || idp == nil {
		var err error
		if c.MetadataXML != "" {
			idp, err = parseIdPMetadata([]byte(c.MetadataXML))
		} else {
			fctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			idp, err = fetchIdPMetadata(fctx, c.httpClient(), c.MetadataURL)
			cancel()
		}
		if err != nil {
			ssoProviders.mu.Unlock()
			return nil, fmt.Errorf("identity provider metadata: %w", err)
		}
		ssoProviders.key, ssoProviders.idp = c.cacheKey(), idp
	}
	ssoProviders.mu.Unlock()
	pair, err := tls.X509KeyPair([]byte(c.SPCert), []byte(c.SPKey))
	if err != nil {
		return nil, fmt.Errorf("the console's SAML key: %w", err)
	}
	cert, _ := x509.ParseCertificate(pair.Certificate[0])
	base := s.baseURL(r)
	meta, _ := url.Parse(base + "/login/sso/metadata")
	acs, _ := url.Parse(base + "/login/sso/acs")
	return &saml.ServiceProvider{EntityID: meta.String(), Key: pair.PrivateKey.(*rsa.PrivateKey), Certificate: cert,
		MetadataURL: *meta, AcsURL: *acs, IDPMetadata: idp, AuthnNameIDFormat: saml.UnspecifiedNameIDFormat}, nil
}

// newSPKey creates the console's SAML signing key and certificate.
func newSPKey() (keyPEM, certPEM string, err error) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", err
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "BackupZit console SAML"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(10, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})),
		string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), nil
}

// ---- sign-in

func (s *Server) handleSSOStart(w http.ResponseWriter, r *http.Request) {
	c := s.ssoSettings(r.Context())
	switch c.Protocol {
	case "oidc":
		oc, _, err := s.oidcConfig(r.Context(), c, r)
		if err != nil {
			s.ssoFailed(w, r, err)
			return
		}
		state, nonce, verifier := randomString(), randomString(), oauth2.GenerateVerifier()
		s.setSSOCookie(w, r, ssoState(state, nonce, verifier))
		http.Redirect(w, r, oc.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), http.StatusSeeOther)
	case "saml":
		sp, err := s.serviceProvider(r.Context(), c, r)
		if err != nil {
			s.ssoFailed(w, r, err)
			return
		}
		req, err := sp.MakeAuthenticationRequest(sp.GetSSOBindingLocation(saml.HTTPRedirectBinding), saml.HTTPRedirectBinding, saml.HTTPPostBinding)
		if err != nil {
			s.ssoFailed(w, r, err)
			return
		}
		relay := randomString()
		u, err := req.Redirect(relay, sp)
		if err != nil {
			s.ssoFailed(w, r, err)
			return
		}
		s.setSSOCookie(w, r, ssoState(req.ID, relay))
		http.Redirect(w, r, u.String(), http.StatusSeeOther)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	c := s.ssoSettings(r.Context())
	ck, err := r.Cookie(ssoCookie)
	if c.Protocol != "oidc" || err != nil {
		s.ssoFailed(w, r, errors.New("the sign-in took too long or was started elsewhere; try again"))
		return
	}
	st, ok := parseSSOState(ck.Value)
	if ok && len(st) == 3 && !firstUse("oidc:"+st[0]) {
		s.ssoFailed(w, r, errors.New("this sign-in was already used; start again"))
		return
	}
	if !ok || len(st) != 3 || r.FormValue("state") != st[0] {
		s.ssoFailed(w, r, errors.New("the sign-in took too long or was started elsewhere; try again"))
		return
	}
	if e := r.FormValue("error"); e != "" {
		s.ssoFailed(w, r, fmt.Errorf("the identity provider refused the sign-in: %s %s", e, r.FormValue("error_description")))
		return
	}
	oc, verifier, err := s.oidcConfig(r.Context(), c, r)
	if err != nil {
		s.ssoFailed(w, r, err)
		return
	}
	tok, err := oc.Exchange(context.WithValue(r.Context(), oauth2.HTTPClient, c.httpClient()), r.FormValue("code"), oauth2.VerifierOption(st[2]))
	if err != nil {
		s.ssoFailed(w, r, fmt.Errorf("token exchange: %w", err))
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := verifier.Verify(r.Context(), raw)
	if err != nil {
		s.ssoFailed(w, r, fmt.Errorf("ID token: %w", err))
		return
	}
	if idt.Nonce != st[1] {
		s.ssoFailed(w, r, errors.New("ID token: wrong nonce"))
		return
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		s.ssoFailed(w, r, err)
		return
	}
	name := firstString(claims, c.UsernameClaim, "preferred_username", "upn", "email", "sub")
	groups := stringList(claims[orDefault(c.GroupsClaim, "groups")])
	s.ssoSignIn(w, r, c, name, firstString(claims, "name"), firstString(claims, "email"), groups)
}

func (s *Server) handleSAMLACS(w http.ResponseWriter, r *http.Request) {
	c := s.ssoSettings(r.Context())
	ck, err := r.Cookie(ssoCookie)
	if c.Protocol != "saml" || err != nil {
		s.ssoFailed(w, r, errors.New("the sign-in took too long or was started elsewhere; try again"))
		return
	}
	st, ok := parseSSOState(ck.Value)
	if !ok || len(st) != 2 {
		s.ssoFailed(w, r, errors.New("the sign-in took too long or was started elsewhere; try again"))
		return
	}
	if !firstUse("saml:" + st[0]) {
		s.ssoFailed(w, r, errors.New("this sign-in was already used; start again"))
		return
	}
	sp, err := s.serviceProvider(r.Context(), c, r)
	if err != nil {
		s.ssoFailed(w, r, err)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.ssoFailed(w, r, err)
		return
	}
	a, err := sp.ParseResponse(r, []string{st[0]})
	if err != nil {
		var ie *saml.InvalidResponseError
		if errors.As(err, &ie) {
			s.log.Warn("SAML response refused", "why", ie.PrivateErr)
		}
		s.ssoFailed(w, r, errors.New("the identity provider's answer was not accepted (see the server log)"))
		return
	}
	attrs := map[string][]string{}
	for _, as := range a.AttributeStatements {
		for _, at := range as.Attributes {
			for _, v := range at.Values {
				attrs[at.Name] = append(attrs[at.Name], v.Value)
				if at.FriendlyName != "" {
					attrs[at.FriendlyName] = append(attrs[at.FriendlyName], v.Value)
				}
			}
		}
	}
	first := func(keys ...string) string {
		for _, k := range keys {
			if k != "" && len(attrs[k]) > 0 && attrs[k][0] != "" {
				return attrs[k][0]
			}
		}
		return ""
	}
	name := first(c.UsernameClaim, "username", "uid", "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/upn", "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/name")
	if name == "" && a.Subject != nil && a.Subject.NameID != nil {
		name = a.Subject.NameID.Value
	}
	groups := attrs[orDefault(c.GroupsClaim, "groups")]
	if len(groups) == 0 {
		groups = attrs["http://schemas.microsoft.com/ws/2008/06/identity/claims/groups"]
	}
	s.ssoSignIn(w, r, c, name, first("displayName", "http://schemas.microsoft.com/identity/claims/displayname"),
		first("email", "mail", "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress"), groups)
}

// handleSAMLMetadata serves the console's SAML metadata for the identity
// provider.
func (s *Server) handleSAMLMetadata(w http.ResponseWriter, r *http.Request) {
	c := s.ssoSettings(r.Context())
	if c.Protocol != "saml" || c.SPCert == "" {
		http.NotFound(w, r)
		return
	}
	pair, err := tls.X509KeyPair([]byte(c.SPCert), []byte(c.SPKey))
	if err != nil {
		s.serverError(w, err)
		return
	}
	cert, _ := x509.ParseCertificate(pair.Certificate[0])
	base := s.baseURL(r)
	meta, _ := url.Parse(base + "/login/sso/metadata")
	acs, _ := url.Parse(base + "/login/sso/acs")
	sp := saml.ServiceProvider{EntityID: meta.String(), Key: pair.PrivateKey.(*rsa.PrivateKey), Certificate: cert, MetadataURL: *meta, AcsURL: *acs}
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	if err := xmlEncode(&b, sp.Metadata()); err != nil {
		s.serverError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/samlmetadata+xml")
	w.Write(b.Bytes())
}

// ssoSignIn creates or updates the user and starts the session.
func (s *Server) ssoSignIn(w http.ResponseWriter, r *http.Request, c SSOSettings, name, display, email string, groups []string) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 200 {
		s.ssoFailed(w, r, errors.New("the identity provider sent no user name"))
		return
	}
	role, err := c.role(groups)
	if err != nil {
		s.auditAs(r, clip(name, 64), "login.failed", "single sign-on: no role for groups "+strings.Join(groups, ", "))
		s.ssoFailed(w, r, err)
		return
	}
	u, err := s.store.upsertDirectoryUser(r.Context(), "sso", name, display, email, role)
	if err != nil {
		s.ssoFailed(w, r, err)
		return
	}
	if u.Source != "sso" {
		s.auditAs(r, clip(name, 64), "login.failed", "single sign-on for a "+u.Source+" account")
		s.ssoFailed(w, r, fmt.Errorf("the user %s exists as a %s account and cannot sign in with single sign-on", name, u.Source))
		return
	}
	if u.Disabled {
		s.ssoFailed(w, r, errors.New("this account is disabled"))
		return
	}
	http.SetCookie(w, &http.Cookie{Name: ssoCookie, Value: "", Path: "/login/sso", MaxAge: -1})
	s.startSession(w, r, u, "single sign-on")
}

func (s *Server) ssoFailed(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Warn("single sign-on failed", "err", err, "remote", s.guard.ClientIP(r))
	w.WriteHeader(http.StatusUnauthorized)
	s.render(w, r, "login", pageData{Title: "Sign in", Error: "Single sign-on failed: " + err.Error()})
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && k != "" && v != "" {
			return v
		}
	}
	return ""
}

// stringList reads a claim that is a list of strings or one string.
func stringList(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func xmlEncode(b *bytes.Buffer, v any) error {
	enc := xml.NewEncoder(b)
	enc.Indent("", "  ")
	return enc.Encode(v)
}

// parseIdPMetadata reads an EntityDescriptor (or the first one of an
// EntitiesDescriptor).
func parseIdPMetadata(b []byte) (*saml.EntityDescriptor, error) {
	var ed saml.EntityDescriptor
	if err := xml.Unmarshal(b, &ed); err == nil && len(ed.IDPSSODescriptors) > 0 {
		return &ed, nil
	}
	var eds saml.EntitiesDescriptor
	if err := xml.Unmarshal(b, &eds); err != nil {
		return nil, err
	}
	for i := range eds.EntityDescriptors {
		if len(eds.EntityDescriptors[i].IDPSSODescriptors) > 0 {
			return &eds.EntityDescriptors[i], nil
		}
	}
	return nil, errors.New("no identity provider in the metadata")
}

func fetchIdPMetadata(ctx context.Context, client *http.Client, u string) (*saml.EntityDescriptor, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", u, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	return parseIdPMetadata(b)
}

// ---- settings page

type ssoPage struct {
	SSOSettings
	HasSecret bool
	Callback  string // OpenID Connect redirect URI to register
	Metadata  string // SAML metadata URL of the console
	ACS       string
}

func (s *Server) ssoPage(r *http.Request) ssoPage {
	c := s.ssoSettings(r.Context())
	p := ssoPage{SSOSettings: c, HasSecret: c.ClientSecret != ""}
	p.ClientSecret, p.SPKey = "", ""
	base := s.baseURL(r)
	p.Callback, p.Metadata, p.ACS = base+"/login/sso/callback", base+"/login/sso/metadata", base+"/login/sso/acs"
	return p
}

func (s *Server) handleSettingsSSO(w http.ResponseWriter, r *http.Request, user string) {
	const back = "/settings/sso"
	old := s.ssoSettings(r.Context())
	t := func(k string) string { return strings.TrimSpace(r.FormValue(k)) }
	c := SSOSettings{Protocol: t("protocol"), Label: t("label"), Issuer: t("issuer"), ClientID: t("client_id"), ClientSecret: r.FormValue("client_secret"),
		MetadataURL: t("metadata_url"), MetadataXML: t("metadata_xml"), CACert: t("ca_cert"), UsernameClaim: t("username_claim"), GroupsClaim: t("groups_claim"),
		AdminGroup: t("admin_group"), OperatorGroup: t("operator_group"), RestoreGroup: t("restore_group"), ViewerGroup: t("viewer_group"),
		DefaultRole: t("default_role"), SPKey: old.SPKey, SPCert: old.SPCert}
	if c.ClientSecret == "" {
		c.ClientSecret = old.ClientSecret // unchanged
	}
	if err := c.Validate(); err != nil {
		redirectErr(w, r, back, err)
		return
	}
	if c.Protocol == "saml" && c.SPKey == "" {
		k, crt, err := newSPKey()
		if err != nil {
			s.serverError(w, err)
			return
		}
		c.SPKey, c.SPCert = k, crt
	}
	// Check the identity provider now, so a typo shows here and not at
	// the next sign-in.
	switch c.Protocol {
	case "oidc":
		if _, _, err := s.oidcConfig(r.Context(), c, r); err != nil {
			redirectErr(w, r, back, err)
			return
		}
	case "saml":
		if _, err := s.serviceProvider(r.Context(), c, r); err != nil {
			redirectErr(w, r, back, err)
			return
		}
	}
	if s.store.fourEyes(r.Context()) {
		// Identity provider groups decide roles, as with LDAP.
		p, err := s.store.sealPayload(c)
		if err != nil {
			s.serverError(w, err)
			return
		}
		if s.needsApproval(w, r, back, "settings.sso", 0, strings.ToUpper(c.Protocol)+" "+c.Issuer+c.MetadataURL, p) {
			return
		}
	}
	if err := s.store.SetSetting(r.Context(), settingSSO, c); err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, "settings.sso", "protocol %q, issuer %q, metadata %q, default role %q", c.Protocol, c.Issuer, c.MetadataURL, c.DefaultRole)
	redirectMsg(w, r, back, "Single sign-on settings saved.")
}

// usedSSOStates remembers sign-in states (OpenID Connect state, SAML
// request ID) already used, so a captured answer cannot be sent again.
var usedSSOStates = struct {
	sync.Mutex
	m map[string]time.Time
}{m: map[string]time.Time{}}

// firstUse marks state as used; it is false when it was used before.
func firstUse(state string) bool {
	usedSSOStates.Lock()
	defer usedSSOStates.Unlock()
	now := time.Now()
	for k, t := range usedSSOStates.m {
		if now.Sub(t) > 15*time.Minute {
			delete(usedSSOStates.m, k)
		}
	}
	if _, ok := usedSSOStates.m[state]; ok {
		return false
	}
	usedSSOStates.m[state] = now
	return true
}

// httpClient reaches the identity provider; with CACert it also trusts
// that certificate (an internal CA or a self-signed identity provider).
func (c SSOSettings) httpClient() *http.Client {
	if c.CACert == "" {
		return &http.Client{Timeout: 30 * time.Second}
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	pool.AppendCertsFromPEM([]byte(c.CACert))
	return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}}
}
