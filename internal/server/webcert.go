package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The console always serves agents on its main port with its own
// self-signed certificate, which agents pin. For browsers it can in
// addition serve a "web address" (usually port 443) with a trusted
// certificate: uploaded by the administrator or obtained from Let's
// Encrypt. Port 80 then redirects to HTTPS and answers ACME challenges.

const settingCert = "certificate"

// CertSettings configure the browser certificate (Settings → Certificate).
type CertSettings struct {
	// Mode is "self" (only the console's own certificate on the main
	// port), "upload" or "acme".
	Mode string `json:"mode"`
	// Hostname is the DNS name browsers use, e.g. backup.example.com.
	Hostname string `json:"hostname,omitempty"`
	// Port for HTTPS with the trusted certificate (default 443).
	Port int `json:"port,omitempty"`
	// RedirectHTTP serves port 80: redirects to HTTPS and answers ACME
	// HTTP-01 challenges.
	RedirectHTTP bool `json:"redirect_http,omitempty"`
	// Let's Encrypt.
	ACMEEmail string `json:"acme_email,omitempty"`
	// Challenge is "http" (port 80 reachable from the internet) or
	// "cloudflare" (DNS TXT record through the Cloudflare API).
	Challenge       string `json:"challenge,omitempty"`
	CloudflareToken string `json:"cloudflare_token,omitempty"`
	// ACMEDirectory overrides the Let's Encrypt directory (staging, tests).
	ACMEDirectory string `json:"acme_directory,omitempty"`
}

const (
	letsEncrypt        = "https://acme-v02.api.letsencrypt.org/directory"
	letsEncryptStaging = "https://acme-staging-v02.api.letsencrypt.org/directory"
)

var hostnameRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

func (c CertSettings) Validate() error {
	switch c.Mode {
	case "self", "":
		return nil
	case "upload", "acme":
	default:
		return errors.New("unknown certificate mode")
	}
	if !hostnameRe.MatchString(c.Hostname) {
		return errors.New("enter the DNS name browsers use for the console, e.g. backup.example.com")
	}
	if c.Port < 1 || c.Port > 65535 {
		return errors.New("invalid HTTPS port")
	}
	if c.RedirectHTTP && c.Port == 80 {
		return errors.New("the HTTPS port cannot be 80 when port 80 redirects to HTTPS")
	}
	if c.Mode == "acme" {
		if !strings.Contains(c.ACMEEmail, "@") || len(c.ACMEEmail) > 200 {
			return errors.New("Let's Encrypt needs an email address for expiry notices")
		}
		switch c.Challenge {
		case "http":
			if !c.RedirectHTTP {
				return errors.New("the HTTP challenge needs port 80: enable \"Serve port 80\"")
			}
		case "cloudflare":
			if c.CloudflareToken == "" {
				return errors.New("enter a Cloudflare API token with Zone.DNS edit permission")
			}
		default:
			return errors.New("choose how Let's Encrypt verifies the name")
		}
	}
	return nil
}

func (c CertSettings) directory() string {
	if c.ACMEDirectory != "" {
		return c.ACMEDirectory
	}
	return letsEncrypt
}

// CertInfo describes a certificate for the settings page.
type CertInfo struct {
	Subject     string
	Issuer      string
	Names       []string
	NotAfter    time.Time
	Fingerprint string
	SelfSigned  bool
}

// DaysLeft until the certificate expires.
func (c CertInfo) DaysLeft() int { return int(time.Until(c.NotAfter).Hours() / 24) }

func certInfo(c *tls.Certificate) *CertInfo {
	if c == nil || len(c.Certificate) == 0 {
		return nil
	}
	x, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return nil
	}
	names := append([]string(nil), x.DNSNames...)
	for _, ip := range x.IPAddresses {
		names = append(names, ip.String())
	}
	return &CertInfo{Subject: x.Subject.CommonName, Issuer: x.Issuer.CommonName, Names: names, NotAfter: x.NotAfter,
		Fingerprint: CertFingerprint(*c), SelfSigned: x.Issuer.String() == x.Subject.String()}
}

// parseKeyPair checks an uploaded certificate chain and key.
func parseKeyPair(certPEM, keyPEM []byte, hostname string, now time.Time) (*tls.Certificate, error) {
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("certificate and key do not fit together or are not PEM: %v", err)
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return nil, err
	}
	if now.After(leaf.NotAfter) {
		return nil, fmt.Errorf("the certificate expired on %s", leaf.NotAfter.Format("2006-01-02"))
	}
	if now.Before(leaf.NotBefore) {
		return nil, fmt.Errorf("the certificate is valid only from %s", leaf.NotBefore.Format("2006-01-02"))
	}
	if hostname != "" {
		if err := leaf.VerifyHostname(hostname); err != nil {
			return nil, fmt.Errorf("the certificate is not valid for %s (it covers %s)", hostname, strings.Join(leaf.DNSNames, ", "))
		}
	}
	c.Leaf = leaf
	return &c, nil
}

// webTLS runs the browser listeners and holds the trusted certificate.
type webTLS struct {
	s   *Server
	dir string // data directory/web
	log *slog.Logger

	mu       sync.Mutex
	cert     *tls.Certificate
	cfg      CertSettings
	https    *http.Server
	http     *http.Server
	lastErr  string
	acmeKeys map[string]string // HTTP-01 token → key authorization
	listen   func(network, addr string) (net.Listener, error)
	// dnsDelay overrides the wait for DNS propagation, cfBase the
	// Cloudflare API address (tests).
	cfBase      string
	dnsDelay    time.Duration
	dnsDelaySet bool
}

func (w *webTLS) certFile() string { return filepath.Join(w.dir, "cert.pem") }
func (w *webTLS) keyFile() string  { return filepath.Join(w.dir, "key.pem") }

// loadCert reads the stored trusted certificate, if any.
func (w *webTLS) loadCert() *tls.Certificate {
	c, err := tls.LoadX509KeyPair(w.certFile(), w.keyFile())
	if err != nil {
		return nil
	}
	c.Leaf, _ = x509.ParseCertificate(c.Certificate[0])
	return &c
}

// saveCert writes a certificate chain and key (key readable only by the service).
func (w *webTLS) saveCert(certPEM, keyPEM []byte) error {
	if err := os.MkdirAll(w.dir, 0o700); err != nil {
		return err
	}
	for _, f := range []struct {
		path string
		data []byte
		mode os.FileMode
	}{{w.keyFile(), keyPEM, 0o600}, {w.certFile(), certPEM, 0o644}} {
		tmp := f.path + ".new"
		if err := os.WriteFile(tmp, f.data, f.mode); err != nil {
			return err
		}
		if err := os.Rename(tmp, f.path); err != nil {
			return err
		}
	}
	return nil
}

func (w *webTLS) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cert == nil {
		return nil, errors.New("no certificate")
	}
	return w.cert, nil
}

// apply (re)starts the browser listeners for cfg.
func (w *webTLS) apply(cfg CertSettings) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	old := w.cfg
	w.cfg = cfg
	if cfg.Mode == "upload" || cfg.Mode == "acme" {
		if c := w.loadCert(); c != nil {
			w.cert = c
		}
	} else {
		w.cert = nil
	}
	listen := w.listen
	if listen == nil {
		listen = net.Listen
	}
	// HTTPS with the trusted certificate.
	wantHTTPS := cfg.Mode != "self" && cfg.Mode != "" && w.cert != nil
	if w.https != nil && (!wantHTTPS || old.Port != cfg.Port) {
		closeServer(w.https)
		w.https = nil
	}
	var errs []string
	if wantHTTPS && w.https == nil {
		ln, err := listen("tcp", ":"+strconv.Itoa(cfg.Port))
		if err != nil {
			errs = append(errs, fmt.Sprintf("HTTPS port %d: %v", cfg.Port, err))
		} else {
			hs := &http.Server{Handler: w.s.Handler(), TLSConfig: &tls.Config{GetCertificate: w.getCertificate, MinVersion: tls.VersionTLS12},
				ReadHeaderTimeout: 15 * time.Second, ReadTimeout: 5 * time.Minute, IdleTimeout: 2 * time.Minute}
			w.https = hs
			go hs.ServeTLS(ln, "", "")
			w.log.Info("serving the console with a trusted certificate", "port", cfg.Port, "host", cfg.Hostname)
		}
	}
	// Port 80: redirect and ACME challenges.
	wantHTTP := cfg.RedirectHTTP && cfg.Mode != "self" && cfg.Mode != ""
	if w.http != nil && !wantHTTP {
		closeServer(w.http)
		w.http = nil
	}
	if wantHTTP && w.http == nil {
		ln, err := listen("tcp", ":80")
		if err != nil {
			errs = append(errs, fmt.Sprintf("port 80: %v", err))
		} else {
			hs := &http.Server{Handler: http.HandlerFunc(w.serveHTTP), ReadHeaderTimeout: 15 * time.Second, ReadTimeout: time.Minute}
			w.http = hs
			go hs.Serve(ln)
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func closeServer(hs *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hs.Shutdown(ctx)
}

// serveHTTP answers ACME HTTP-01 challenges and redirects everything else
// to the HTTPS address.
func (w *webTLS) serveHTTP(rw http.ResponseWriter, r *http.Request) {
	if tok, ok := strings.CutPrefix(r.URL.Path, "/.well-known/acme-challenge/"); ok {
		w.mu.Lock()
		ka, found := w.acmeKeys[tok]
		w.mu.Unlock()
		if !found {
			http.NotFound(rw, r)
			return
		}
		rw.Header().Set("Content-Type", "text/plain")
		rw.Write([]byte(ka))
		return
	}
	w.mu.Lock()
	cfg := w.cfg
	w.mu.Unlock()
	target := "https://" + cfg.Hostname
	if cfg.Port != 443 {
		target += ":" + strconv.Itoa(cfg.Port)
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(rw, "use HTTPS: "+target, http.StatusBadRequest)
		return
	}
	http.Redirect(rw, r, target+r.URL.RequestURI(), http.StatusMovedPermanently)
}

// status returns the current trusted certificate and the last error.
func (w *webTLS) status() (*CertInfo, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return certInfo(w.cert), w.lastErr
}

func (w *webTLS) setErr(msg string) {
	w.mu.Lock()
	w.lastErr = msg
	w.mu.Unlock()
}

// WebAddress is the browser URL of the console with the trusted
// certificate, or "" when only the main port is used.
func (w *webTLS) WebAddress() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.https == nil {
		return ""
	}
	u := "https://" + w.cfg.Hostname
	if w.cfg.Port != 443 {
		u += ":" + strconv.Itoa(w.cfg.Port)
	}
	return u
}

// pemEncode returns a PEM chain for a DER chain.
func pemEncode(der [][]byte) []byte {
	var b []byte
	for _, d := range der {
		b = append(b, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: d})...)
	}
	return b
}
