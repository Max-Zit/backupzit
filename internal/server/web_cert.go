package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// StartWeb loads the browser certificate settings, starts the extra
// listeners and renews Let's Encrypt certificates in the background.
func (s *Server) StartWeb(ctx context.Context, dataDir string) {
	s.web = &webTLS{s: s, dir: filepath.Join(dataDir, "web"), log: s.log}
	cfg := s.certSettings(ctx)
	if err := s.web.apply(cfg); err != nil {
		s.log.Error("browser certificate", "err", err)
		s.web.setErr(err.Error())
	}
	go func() {
		for {
			s.warnExpiringCert(ctx)
			if err := s.web.renewIfNeeded(ctx); err != nil {
				s.log.Error("renew Let's Encrypt certificate", "err", err)
				s.mailAdmins(ctx, "[BackupZit] Certificate renewal failed",
					"BackupZit could not renew the Let's Encrypt certificate of the console:\n\n"+err.Error()+
						"\n\nThe current certificate stays in use until it expires. Check Settings → Certificate.\n")
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(12 * time.Hour):
			}
		}
	}()
}

func (s *Server) certSettings(ctx context.Context) CertSettings {
	c := CertSettings{Mode: "self", Port: 443, RedirectHTTP: true, Challenge: "http"}
	if err := s.store.GetSetting(ctx, settingCert, &c); err != nil {
		s.log.Error("certificate settings", "err", err)
	}
	if c.Port == 0 {
		c.Port = 443
	}
	return c
}

// mailAdmins emails the notification recipients, if email is set up.
func (s *Server) mailAdmins(ctx context.Context, subject, body string) {
	var e EmailSettings
	if err := s.store.GetSetting(ctx, settingEmail, &e); err != nil || !e.Enabled || e.Host == "" || len(e.To) == 0 {
		return
	}
	if err := sendMail(ctx, e, subject, body); err != nil {
		s.log.Error("email", "subject", subject, "err", err)
	}
}

// readUpload reads a form file or text field with a PEM block.
func readUpload(r *http.Request, name string) []byte {
	if f, _, err := r.FormFile(name + "_file"); err == nil {
		defer f.Close()
		if b, err := io.ReadAll(io.LimitReader(f, 1<<20)); err == nil && len(b) > 0 {
			return b
		}
	}
	return []byte(strings.TrimSpace(r.FormValue(name)))
}

func (s *Server) handleSettingsCertificate(w http.ResponseWriter, r *http.Request, user string) {
	const back = "/settings/certificate"
	if s.web == nil {
		redirectErr(w, r, back, errors.New("the certificate cannot be changed in this mode"))
		return
	}
	r.ParseMultipartForm(4 << 20)
	old := s.certSettings(r.Context())
	port, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("port")))
	c := CertSettings{
		Mode:         r.FormValue("mode"),
		Hostname:     strings.ToLower(strings.TrimSpace(r.FormValue("hostname"))),
		Port:         port,
		RedirectHTTP: r.FormValue("redirect_http") == "on",
		ACMEEmail:    strings.TrimSpace(r.FormValue("acme_email")),
		Challenge:    r.FormValue("challenge"),
	}
	c.CloudflareToken = strings.TrimSpace(r.FormValue("cloudflare_token"))
	if c.CloudflareToken == "" {
		c.CloudflareToken = old.CloudflareToken
	}
	if r.FormValue("staging") == "on" {
		c.ACMEDirectory = letsEncryptStaging
	}
	if err := c.Validate(); err != nil {
		redirectErr(w, r, back, err)
		return
	}
	switch c.Mode {
	case "upload":
		certPEM, keyPEM := readUpload(r, "cert_pem"), readUpload(r, "key_pem")
		if len(certPEM) == 0 && len(keyPEM) == 0 && old.Mode == "upload" && s.web.loadCert() != nil {
			// Keep the uploaded certificate, only other settings change.
			if cur := s.web.loadCert(); cur.Leaf != nil && cur.Leaf.VerifyHostname(c.Hostname) != nil {
				redirectErr(w, r, back, fmt.Errorf("the uploaded certificate is not valid for %s; upload one that is", c.Hostname))
				return
			}
			break
		}
		cert, err := parseKeyPair(certPEM, keyPEM, c.Hostname, time.Now())
		if err != nil {
			redirectErr(w, r, back, err)
			return
		}
		if err := s.web.saveCert(certPEM, keyPEM); err != nil {
			s.serverError(w, err)
			return
		}
		s.audit(r, "settings.certificate", "uploaded certificate for %s (%s, valid until %s)", c.Hostname, cert.Leaf.Issuer.CommonName, cert.Leaf.NotAfter.Format("2006-01-02"))
	case "acme":
		// Port 80 must already answer for the HTTP challenge.
		if c.Challenge == "http" {
			if err := s.web.apply(CertSettings{Mode: "acme", Hostname: c.Hostname, Port: c.Port, RedirectHTTP: true}); err != nil {
				s.web.apply(old)
				redirectErr(w, r, back, err)
				return
			}
		}
		if err := s.web.obtainACME(r.Context(), c, nil); err != nil {
			s.web.apply(old)
			s.web.setErr(err.Error())
			redirectErr(w, r, back, err)
			return
		}
		s.audit(r, "settings.certificate", "Let's Encrypt certificate for %s (%s challenge)", c.Hostname, c.Challenge)
	default:
		c = CertSettings{Mode: "self", Port: 443, Challenge: "http"}
		s.audit(r, "settings.certificate", "only the console's own certificate")
	}
	if err := s.store.SetSetting(r.Context(), settingCert, c); err != nil {
		s.serverError(w, err)
		return
	}
	s.web.setErr("")
	if err := s.web.apply(c); err != nil {
		s.web.setErr(err.Error())
		redirectErr(w, r, back, fmt.Errorf("saved, but %v", err))
		return
	}
	msg := "Only the console's own certificate is used."
	if c.Mode != "self" {
		msg = "Certificate installed. The console is now available at " + s.web.WebAddress() + "."
	}
	redirectMsg(w, r, back, msg)
}

func (s *Server) handleCertificateRenew(w http.ResponseWriter, r *http.Request, _ string) {
	const back = "/settings/certificate"
	c := s.certSettings(r.Context())
	if c.Mode != "acme" || s.web == nil {
		redirectErr(w, r, back, errors.New("no Let's Encrypt certificate is configured"))
		return
	}
	if err := s.web.obtainACME(r.Context(), c, nil); err != nil {
		s.web.setErr(err.Error())
		redirectErr(w, r, back, err)
		return
	}
	s.web.setErr("")
	s.web.apply(c)
	s.audit(r, "settings.certificate", "renewed Let's Encrypt certificate for %s", c.Hostname)
	redirectMsg(w, r, back, "Certificate renewed.")
}

// certPage is the certificate form's data; the Cloudflare token is not shown.
type certPage struct {
	CertSettings
	HasToken bool
	Staging  bool
}

func certForPage(c CertSettings) certPage {
	p := certPage{CertSettings: c, HasToken: c.CloudflareToken != "", Staging: c.ACMEDirectory == letsEncryptStaging}
	p.CloudflareToken = ""
	return p
}

// warnExpiringCert emails the administrators when an uploaded certificate
// expires within 21 days (Let's Encrypt certificates renew themselves).
func (s *Server) warnExpiringCert(ctx context.Context) {
	c := s.certSettings(ctx)
	info, _ := s.web.status()
	if c.Mode != "upload" || info == nil || info.DaysLeft() >= 21 {
		return
	}
	s.mailAdmins(ctx, "[BackupZit] Console certificate expires in "+strconv.Itoa(info.DaysLeft())+" days",
		fmt.Sprintf("The certificate of %s (issued by %s) expires on %s.\n\nUpload a new one under Settings → Certificate.\n",
			c.Hostname, info.Issuer, info.NotAfter.Format("2006-01-02")))
}
