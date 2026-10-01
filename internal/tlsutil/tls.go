// Package tlsutil creates self-signed certificates and pins them by
// fingerprint (trust on first use), as used by the console and the
// hardened repository.
package tlsutil

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// LoadOrCreateCert loads cert.pem/key.pem from dir, creating a self-signed
// certificate valid for hosts (DNS names or IPs) if none exists.
func LoadOrCreateCert(dir string, hosts []string) (tls.Certificate, error) {
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	if _, err := os.Stat(certPath); errors.Is(err, fs.ErrNotExist) {
		if err := createSelfSigned(certPath, keyPath, hosts); err != nil {
			return tls.Certificate{}, err
		}
	}
	return tls.LoadX509KeyPair(certPath, keyPath)
}

func createSelfSigned(certPath, keyPath string, hosts []string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "backupzit", Organization: []string{"backupzit"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(20, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	if hn, err := os.Hostname(); err == nil {
		hosts = append(hosts, hn)
	}
	hosts = append(hosts, "localhost", "127.0.0.1", "::1")
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if h != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(certPath), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		return err
	}
	return os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

// CertFingerprint returns "SHA256:<base64>" of the leaf certificate, which
// clients pin.
func CertFingerprint(c tls.Certificate) string {
	if len(c.Certificate) == 0 {
		return ""
	}
	return FingerprintDER(c.Certificate[0])
}

// FingerprintDER formats the SHA-256 of a DER certificate.
func FingerprintDER(der []byte) string {
	h := sha256.Sum256(der)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(h[:])
}

// ErrFingerprintMismatch means the server is not the one that was pinned.
var ErrFingerprintMismatch = errors.New("server certificate fingerprint mismatch")

// PinnedConfig returns a client TLS config that accepts only a server
// certificate with the given fingerprint.
func PinnedConfig(fingerprint string) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // verified by VerifyConnection below
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("server presented no certificate")
			}
			if got := FingerprintDER(cs.PeerCertificates[0].Raw); got != fingerprint {
				return fmt.Errorf("%w: expected %s, got %s", ErrFingerprintMismatch, fingerprint, got)
			}
			return nil
		},
	}
}

// FetchFingerprint connects to host:port and returns the fingerprint of the
// certificate it presents, for confirmation by a human.
func FetchFingerprint(ctx context.Context, addr string) (string, error) {
	d := tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true}} // fingerprint is shown to the operator
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return "", errors.New("server presented no certificate")
	}
	return FingerprintDER(certs[0].Raw), nil
}
