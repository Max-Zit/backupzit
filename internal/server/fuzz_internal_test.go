package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Parsers of what browsers, identity providers and uploads send.

func FuzzParseSchedule(f *testing.F) {
	f.Add("0 22 * * 1-5")
	f.Add(`{"kind":"daily","times":["22:00"],"days":[1,2,3]}`)
	f.Add(`{"kind":"interval","every":15,"from":8,"to":18}`)
	f.Add("{")
	f.Fuzz(func(t *testing.T, s string) {
		sc, err := ParseSchedule(s)
		if err != nil {
			return
		}
		if _, err := ParseSchedule(sc.Encode()); err != nil {
			t.Fatalf("%q parsed, but its encoding %q does not: %v", s, sc.Encode(), err)
		}
		_ = sc.Describe()
	})
}

// localPath guards redirects: what it accepts must stay on this console.
func FuzzLocalPath(f *testing.F) {
	for _, s := range []string{"/jobs", "//evil.example", `/\evil.example`, "/\t/evil", "https://evil.example", "/a?b=c#d"} {
		f.Add(s)
	}
	base, _ := url.Parse("https://console.example:8443/settings")
	f.Fuzz(func(t *testing.T, p string) {
		if !localPath(p) {
			return
		}
		u, err := base.Parse(p)
		if err != nil {
			return
		}
		if u.Host != base.Host || u.Scheme != base.Scheme {
			t.Fatalf("%q leads to %s", p, u)
		}
	})
}

func FuzzParseSSOState(f *testing.F) {
	f.Add(ssoState("oidc", "nonce"))
	f.Add("x.y")
	f.Fuzz(func(t *testing.T, tok string) {
		payload, sig, _ := strings.Cut(tok, ".")
		if _, ok := parseSSOState(tok); ok && sig != sign("sso:"+payload) {
			t.Fatalf("state %q without a valid signature accepted", tok)
		}
	})
}

func FuzzParseMFAToken(f *testing.F) {
	now := time.Now()
	f.Add(mfaToken(7, now.Add(time.Minute)))
	f.Add("1.2.3")
	f.Fuzz(func(t *testing.T, tok string) {
		i := strings.LastIndex(tok, ".")
		if _, ok := parseMFAToken(tok, now); ok && (i < 0 || tok[i+1:] != sign("mfa:"+tok[:i])) {
			t.Fatalf("token %q without a valid signature accepted", tok)
		}
	})
}

// sign is the HMAC of the console's flash key, as the parsers check it.
func sign(s string) string {
	m := hmac.New(sha256.New, flashKey)
	m.Write([]byte(s))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func FuzzParseIdPMetadata(f *testing.F) {
	f.Add([]byte(`<EntityDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata" entityID="https://idp.example"><IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol"><SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="https://idp.example/sso"/></IDPSSODescriptor></EntityDescriptor>`))
	f.Add([]byte(`<EntitiesDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata"></EntitiesDescriptor>`))
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = parseIdPMetadata(b)
	})
}

func FuzzParseKeyPair(f *testing.F) {
	f.Add([]byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"), []byte("-----BEGIN PRIVATE KEY-----\nMIIB\n-----END PRIVATE KEY-----\n"))
	f.Fuzz(func(t *testing.T, cert, key []byte) {
		_, _ = parseKeyPair(cert, key, "backup.example", time.Now())
	})
}

// Recovery keys are typed or pasted into the storage form.
func FuzzParseRecoveryKey(f *testing.F) {
	f.Add("K7QM-2XRD-AAAA-BBBB-CCCC-DDDD-EEEE-FFFF")
	f.Add("K7QM2XRDAAAABBBBCCCCDDDD\r\nEEEEFFFF\r\n\n")
	f.Fuzz(func(t *testing.T, s string) {
		k, err := parseRecoveryKey(s)
		if err != nil {
			return
		}
		if len(k) != 39 {
			t.Fatalf("%q gave key %q", s, k)
		}
		if k2, err := parseRecoveryKey(k); err != nil || k2 != k {
			t.Fatalf("key %q does not parse to itself: %q %v", k, k2, err)
		}
	})
}
