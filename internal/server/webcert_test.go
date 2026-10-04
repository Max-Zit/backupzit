package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/letsencrypt/pebble/v2/ca"
	"github.com/letsencrypt/pebble/v2/db"
	"github.com/letsencrypt/pebble/v2/va"
	"github.com/letsencrypt/pebble/v2/wfe"
)

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newTestCA(t *testing.T) *testCA {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Test CA"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(c)
	return &testCA{c, key, pool}
}

// leaf issues a certificate for names, valid between from and to.
func (ca *testCA) leaf(t *testing.T, names []string, from, to time.Time) (certPEM, keyPEM []byte) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: names[0]},
		NotBefore: from, NotAfter: to, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, n)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	return pemEncode([][]byte{der, ca.cert.Raw}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
}

func TestParseKeyPair(t *testing.T) {
	ca := newTestCA(t)
	now := time.Now()
	c, k := ca.leaf(t, []string{"backup.example.com"}, now.Add(-time.Hour), now.Add(90*24*time.Hour))
	if _, err := parseKeyPair(c, k, "backup.example.com", now); err != nil {
		t.Fatal(err)
	}
	if _, err := parseKeyPair(c, k, "other.example.com", now); err == nil || !strings.Contains(err.Error(), "not valid for other.example.com") {
		t.Errorf("wrong name: %v", err)
	}
	wc, wk := ca.leaf(t, []string{"*.example.com"}, now.Add(-time.Hour), now.Add(time.Hour))
	if _, err := parseKeyPair(wc, wk, "backup.example.com", now); err != nil {
		t.Errorf("wildcard: %v", err)
	}
	if _, err := parseKeyPair(c, wk, "backup.example.com", now); err == nil {
		t.Error("mismatched key accepted")
	}
	if _, err := parseKeyPair(c, k, "backup.example.com", now.Add(100*24*time.Hour)); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("expired: %v", err)
	}
	// Networks without internal DNS: a certificate for an IP address.
	ic, ik := ca.leaf(t, []string{"192.168.1.20"}, now.Add(-time.Hour), now.Add(time.Hour))
	if _, err := parseKeyPair(ic, ik, "192.168.1.20", now); err != nil {
		t.Errorf("IP certificate: %v", err)
	}
	if _, err := parseKeyPair(ic, ik, "192.168.1.21", now); err == nil || !strings.Contains(err.Error(), "covers 192.168.1.20") {
		t.Errorf("other IP: %v", err)
	}
	if err := (CertSettings{Mode: "upload", Hostname: "192.168.1.20", Port: 443}).Validate(); err != nil {
		t.Errorf("IP address as hostname refused: %v", err)
	}
	for _, bad := range []CertSettings{
		{Mode: "acme", Hostname: "192.168.1.20", Port: 443, ACMEEmail: "a@b.c", Challenge: "http", RedirectHTTP: true},
		{Mode: "upload", Hostname: "not a name", Port: 443},
		{Mode: "upload", Hostname: "a.example.com", Port: 0},
		{Mode: "acme", Hostname: "a.example.com", Port: 443, ACMEEmail: "x", Challenge: "http", RedirectHTTP: true},
		{Mode: "acme", Hostname: "a.example.com", Port: 443, ACMEEmail: "a@b.c", Challenge: "http"},
		{Mode: "acme", Hostname: "a.example.com", Port: 443, ACMEEmail: "a@b.c", Challenge: "cloudflare"},
		{Mode: "upload", Hostname: "a.example.com", Port: 80, RedirectHTTP: true},
	} {
		if bad.Validate() == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

// testWeb returns a webTLS whose listeners use free local ports.
func testWeb(t *testing.T) (*webTLS, func(port string) string) {
	srv, err := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	addrs := map[string]string{}
	w := &webTLS{s: srv, dir: t.TempDir(), log: srv.log, dnsDelaySet: true,
		listen: func(network, addr string) (net.Listener, error) {
			ln, err := net.Listen(network, "127.0.0.1:0")
			if err == nil {
				mu.Lock()
				addrs[addr] = ln.Addr().String()
				mu.Unlock()
			}
			return ln, err
		}}
	t.Cleanup(func() { w.apply(CertSettings{Mode: "self"}) })
	return w, func(port string) string { mu.Lock(); defer mu.Unlock(); return addrs[port] }
}

func TestWebTLSUpload(t *testing.T) {
	ca := newTestCA(t)
	w, addr := testWeb(t)
	c, k := ca.leaf(t, []string{"backup.example.com"}, time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour))
	if err := w.saveCert(c, k); err != nil {
		t.Fatal(err)
	}
	cfg := CertSettings{Mode: "upload", Hostname: "backup.example.com", Port: 443, RedirectHTTP: true}
	if err := w.apply(cfg); err != nil {
		t.Fatal(err)
	}
	cl := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: ca.pool, ServerName: "backup.example.com"}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := cl.Get("https://" + addr(":443") + "/login")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "Sign in") {
		t.Fatalf("HTTPS login page: %d", resp.StatusCode)
	}
	resp, err = cl.Get("http://" + addr(":80") + "/runs/5?x=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 301 || resp.Header.Get("Location") != "https://backup.example.com/runs/5?x=1" {
		t.Errorf("redirect: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	w.mu.Lock()
	w.acmeKeys = map[string]string{"tok": "tok.thumb"}
	w.mu.Unlock()
	resp, _ = cl.Get("http://" + addr(":80") + "/.well-known/acme-challenge/tok")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "tok.thumb" {
		t.Errorf("challenge answer %q", b)
	}
	if info, _ := w.status(); info == nil || info.Subject != "backup.example.com" || w.WebAddress() != "https://backup.example.com" {
		t.Errorf("status %+v %s", info, w.WebAddress())
	}
	// Back to the console certificate only: the listeners stop.
	https := addr(":443")
	if err := w.apply(CertSettings{Mode: "self"}); err != nil {
		t.Fatal(err)
	}
	if _, err := net.DialTimeout("tcp", https, time.Second); err == nil {
		t.Error("HTTPS listener still open")
	}
}

// startPebble runs Let's Encrypt's test CA in-process; challenges always pass.
func startPebble(t *testing.T) *httptest.Server {
	t.Setenv("PEBBLE_VA_ALWAYS_VALID", "1")
	t.Setenv("PEBBLE_VA_NOSLEEP", "1")
	t.Setenv("PEBBLE_WFE_NONCEREJECT", "0")
	logger := log.New(io.Discard, "", 0)
	store := db.NewMemoryStore()
	c := ca.New(logger, store, "", "ecdsa", 0, 1, map[string]ca.Profile{"default": {Description: "default"}})
	v := va.New(logger, 5002, 5001, false, "", store)
	w := wfe.New(logger, store, v, c, nil, false, false, 0, 0)
	ts := httptest.NewTLSServer(w.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestACMEWithPebble(t *testing.T) {
	ts := startPebble(t)
	w, _ := testWeb(t)
	ctx := context.Background()
	cfg := CertSettings{Mode: "acme", Hostname: "backup.example.com", Port: 443, RedirectHTTP: true, ACMEEmail: "admin@example.com",
		Challenge: "http", ACMEDirectory: ts.URL + "/dir"}
	if err := w.obtainACME(ctx, cfg, ts.Client()); err != nil {
		t.Fatal(err)
	}
	c := w.loadCert()
	if c == nil || c.Leaf.DNSNames[0] != "backup.example.com" || !strings.Contains(c.Leaf.Issuer.CommonName, "Pebble") {
		t.Fatalf("certificate: %+v", c)
	}

	// DNS challenge through a fake Cloudflare API.
	var records []string
	var mu sync.Mutex
	cf := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer cf-token" {
			json.NewEncoder(rw).Encode(map[string]any{"success": false, "errors": []map[string]string{{"message": "bad token"}}})
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/zones":
			res := []map[string]string{}
			if r.URL.Query().Get("name") == "example.com" {
				res = append(res, map[string]string{"id": "z1"})
			}
			json.NewEncoder(rw).Encode(map[string]any{"success": true, "result": res})
		case r.Method == "POST" && r.URL.Path == "/zones/z1/dns_records":
			var rec map[string]any
			json.NewDecoder(r.Body).Decode(&rec)
			records = append(records, fmt.Sprint("add ", rec["name"]))
			json.NewEncoder(rw).Encode(map[string]any{"success": true, "result": map[string]string{"id": "r1"}})
		case r.Method == "DELETE" && r.URL.Path == "/zones/z1/dns_records/r1":
			records = append(records, "delete")
			json.NewEncoder(rw).Encode(map[string]any{"success": true, "result": map[string]string{"id": "r1"}})
		default:
			http.NotFound(rw, r)
		}
	}))
	defer cf.Close()
	w.cfBase = cf.URL
	cfg.Hostname, cfg.Challenge, cfg.CloudflareToken = "intern.example.com", "cloudflare", "cf-token"
	// Pebble's client trusts its own server; Cloudflare's fake is plain HTTP.
	if err := w.obtainACME(ctx, cfg, ts.Client()); err != nil {
		t.Fatal(err)
	}
	if c := w.loadCert(); c == nil || c.Leaf.DNSNames[0] != "intern.example.com" {
		t.Fatal("no certificate from the DNS challenge")
	}
	if strings.Join(records, ",") != "add _acme-challenge.intern.example.com,delete" {
		t.Errorf("Cloudflare calls: %v", records)
	}
	cfg.CloudflareToken, cfg.Hostname = "wrong", "other.example.com" // a new name needs a new check
	if err := w.obtainACME(ctx, cfg, ts.Client()); err == nil || !strings.Contains(err.Error(), "bad token") {
		t.Errorf("bad Cloudflare token: %v", err)
	}
}
