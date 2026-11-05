package update

import (
	"crypto/ed25519"
	"encoding/base64"
	"testing"
)

// Release manifests and signatures are downloaded: nothing unsigned may
// pass, and malformed input gives an error.
func FuzzVerify(f *testing.F) {
	pub, key, _ := ed25519.GenerateKey(nil)
	keys := []string{base64.StdEncoding.EncodeToString(pub)}
	good := []byte(`{"version":"1.2.3","files":[]}`)
	f.Add(good, Sign(good, key))
	f.Add([]byte(`{}`), "AAAA")
	f.Fuzz(func(t *testing.T, manifest []byte, sig string) {
		m, err := Verify(manifest, sig, keys)
		if err == nil && (m == nil || Verify2(manifest, sig, pub) != nil) {
			t.Fatal("accepted a manifest that is not signed with the key")
		}
	})
}

// Verify2 checks the signature directly, independent of Verify.
func Verify2(manifest []byte, sig string, pub ed25519.PublicKey) error {
	s, err := base64.StdEncoding.DecodeString(trimSpace(sig))
	if err != nil || len(s) != ed25519.SignatureSize || !ed25519.Verify(pub, manifest, s) {
		return errBadSig
	}
	return nil
}

func FuzzNewer(f *testing.F) {
	f.Add("0.47.1", "0.47.0")
	f.Add("1.x", "")
	f.Fuzz(func(t *testing.T, a, b string) {
		if Newer(a, b) && Newer(b, a) {
			t.Fatalf("%q and %q are both newer than each other", a, b)
		}
	})
}

var errBadSig = errorString("bad signature")

type errorString string

func (e errorString) Error() string { return string(e) }

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\n' || s[0] == '\r' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}
