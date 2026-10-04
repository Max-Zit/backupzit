package server

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/acme"
)

// renewBefore is when a Let's Encrypt certificate is renewed.
const renewBefore = 30 * 24 * time.Hour

// accountKey loads or creates the ACME account key.
func (w *webTLS) accountKey() (crypto.Signer, error) {
	p := filepath.Join(w.dir, "acme-account.key")
	if b, err := os.ReadFile(p); err == nil {
		blk, _ := pem.Decode(b)
		if blk == nil {
			return nil, errors.New("invalid ACME account key")
		}
		k, err := x509.ParseECPrivateKey(blk.Bytes)
		if err != nil {
			return nil, err
		}
		return k, nil
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(w.dir, 0o700); err != nil {
		return nil, err
	}
	return k, os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600)
}

// obtainACME gets a certificate for cfg.Hostname from Let's Encrypt and
// installs it.
func (w *webTLS) obtainACME(ctx context.Context, cfg CertSettings, httpClient *http.Client) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	key, err := w.accountKey()
	if err != nil {
		return err
	}
	cl := &acme.Client{Key: key, DirectoryURL: cfg.directory(), UserAgent: "backupzit"}
	if httpClient != nil {
		cl.HTTPClient = httpClient
	}
	acct := &acme.Account{Contact: []string{"mailto:" + cfg.ACMEEmail}}
	if _, err := cl.Register(ctx, acct, acme.AcceptTOS); err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
		return fmt.Errorf("Let's Encrypt account: %w", err)
	}
	order, err := cl.AuthorizeOrder(ctx, acme.DomainIDs(cfg.Hostname))
	if err != nil {
		return fmt.Errorf("order: %w", err)
	}
	for _, u := range order.AuthzURLs {
		z, err := cl.GetAuthorization(ctx, u)
		if err != nil {
			return err
		}
		if z.Status == acme.StatusValid {
			continue
		}
		if err := w.solve(ctx, cl, cfg, z, httpClient); err != nil {
			return err
		}
	}
	finalize, orderURI := order.FinalizeURL, order.URI
	if order, err = cl.WaitOrder(ctx, order.URI); err != nil {
		return fmt.Errorf("order: %w", err)
	}
	if order.FinalizeURL != "" {
		finalize = order.FinalizeURL
	}
	certKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: cfg.Hostname}, DNSNames: []string{cfg.Hostname}}, certKey)
	if err != nil {
		return err
	}
	der, _, err := cl.CreateOrderCert(ctx, finalize, csr, true)
	if err != nil && orderURI != "" {
		// Some ACME servers answer the finalize request without the order's
		// address; follow the order by its original address instead.
		if o, werr := cl.WaitOrder(ctx, orderURI); werr == nil && o.CertURL != "" {
			der, err = cl.FetchCert(ctx, o.CertURL, true)
		}
	}
	if err != nil {
		return fmt.Errorf("issue certificate: %w", err)
	}
	kb, err := x509.MarshalECPrivateKey(certKey)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	certPEM := pemEncode(der)
	if _, err := parseKeyPair(certPEM, keyPEM, cfg.Hostname, time.Now()); err != nil {
		return err
	}
	return w.saveCert(certPEM, keyPEM)
}

// solve completes one authorization with the configured challenge.
func (w *webTLS) solve(ctx context.Context, cl *acme.Client, cfg CertSettings, z *acme.Authorization, httpClient *http.Client) error {
	want := "http-01"
	if cfg.Challenge == "cloudflare" {
		want = "dns-01"
	}
	var ch *acme.Challenge
	for _, c := range z.Challenges {
		if c.Type == want {
			ch = c
		}
	}
	if ch == nil {
		return fmt.Errorf("Let's Encrypt offers no %s challenge for %s", want, z.Identifier.Value)
	}
	switch want {
	case "http-01":
		ka, err := cl.HTTP01ChallengeResponse(ch.Token)
		if err != nil {
			return err
		}
		w.mu.Lock()
		if w.acmeKeys == nil {
			w.acmeKeys = map[string]string{}
		}
		w.acmeKeys[ch.Token] = ka
		w.mu.Unlock()
		defer func() {
			w.mu.Lock()
			delete(w.acmeKeys, ch.Token)
			w.mu.Unlock()
		}()
	case "dns-01":
		val, err := cl.DNS01ChallengeRecord(ch.Token)
		if err != nil {
			return err
		}
		cf := cloudflare{token: cfg.CloudflareToken, http: httpClient, base: w.cfBase}
		name := "_acme-challenge." + z.Identifier.Value
		zone, rec, err := cf.addTXT(ctx, name, val)
		if err != nil {
			return fmt.Errorf("Cloudflare: %w", err)
		}
		defer cf.delete(context.Background(), zone, rec)
		// Give the record time to reach Cloudflare's name servers.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(w.dnsWait()):
		}
	}
	if _, err := cl.Accept(ctx, ch); err != nil {
		return fmt.Errorf("challenge: %w", err)
	}
	if _, err := cl.WaitAuthorization(ctx, z.URI); err != nil {
		hint := "is port 80 of this server reachable from the internet under " + z.Identifier.Value + "?"
		if want == "dns-01" {
			hint = "does the Cloudflare token manage the DNS zone of " + z.Identifier.Value + "?"
		}
		return fmt.Errorf("Let's Encrypt could not verify %s (%s): %w", z.Identifier.Value, hint, err)
	}
	return nil
}

func (w *webTLS) dnsWait() time.Duration {
	if w.dnsDelay > 0 || w.dnsDelaySet {
		return w.dnsDelay
	}
	return 20 * time.Second
}

// renewIfNeeded renews a Let's Encrypt certificate that expires soon.
func (w *webTLS) renewIfNeeded(ctx context.Context) error {
	w.mu.Lock()
	cfg, cur := w.cfg, w.cert
	w.mu.Unlock()
	if cfg.Mode != "acme" {
		return nil
	}
	if cur != nil && cur.Leaf != nil && time.Until(cur.Leaf.NotAfter) > renewBefore {
		return nil
	}
	if err := w.obtainACME(ctx, cfg, nil); err != nil {
		w.setErr(err.Error())
		return err
	}
	w.setErr("")
	return w.apply(cfg)
}

// cloudflare manages DNS TXT records through the Cloudflare API.
type cloudflare struct {
	token string
	http  *http.Client
	base  string // API base, for tests
}

func (c cloudflare) do(ctx context.Context, method, path string, body any, out any) error {
	base := c.base
	if base == "" {
		base = "https://api.cloudflare.com/client/v4"
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	hc := c.http
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var env struct {
		Success bool                       `json:"success"`
		Errors  []struct{ Message string } `json:"errors"`
		Result  json.RawMessage            `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&env); err != nil {
		return fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	if !env.Success {
		var msgs []string
		for _, e := range env.Errors {
			msgs = append(msgs, e.Message)
		}
		return fmt.Errorf("%s", strings.Join(msgs, "; "))
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

// zoneFor finds the Cloudflare zone that holds name.
func (c cloudflare) zoneFor(ctx context.Context, name string) (string, error) {
	labels := strings.Split(strings.TrimSuffix(name, "."), ".")
	for i := 0; i < len(labels)-1; i++ {
		zone := strings.Join(labels[i:], ".")
		var zs []struct{ ID string }
		if err := c.do(ctx, "GET", "/zones?name="+url.QueryEscape(zone), nil, &zs); err != nil {
			return "", err
		}
		if len(zs) > 0 {
			return zs[0].ID, nil
		}
	}
	return "", fmt.Errorf("no Cloudflare zone for %s is accessible with this token", name)
}

func (c cloudflare) addTXT(ctx context.Context, name, value string) (zone, id string, err error) {
	if zone, err = c.zoneFor(ctx, name); err != nil {
		return "", "", err
	}
	var rec struct{ ID string }
	err = c.do(ctx, "POST", "/zones/"+zone+"/dns_records", map[string]any{"type": "TXT", "name": name, "content": value, "ttl": 60}, &rec)
	return zone, rec.ID, err
}

func (c cloudflare) delete(ctx context.Context, zone, id string) error {
	if zone == "" || id == "" {
		return nil
	}
	return c.do(ctx, "DELETE", "/zones/"+zone+"/dns_records/"+id, nil, nil)
}
