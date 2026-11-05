package api

import "testing"

// Enrollment codes are pasted by people (and may be damaged on the way):
// any text must decode to an error or to the values it was built from.
func FuzzDecodeEnrollCode(f *testing.F) {
	f.Add(EncodeEnrollCode("https://backup.example:8443", "token", "SHA256:abc"))
	f.Add("BZ1-")
	f.Add("BZ1-e30")
	f.Fuzz(func(t *testing.T, code string) {
		srv, tok, fp, err := DecodeEnrollCode(code)
		if err != nil {
			return
		}
		if s2, t2, f2, err := DecodeEnrollCode(EncodeEnrollCode(srv, tok, fp)); err != nil || s2 != srv || t2 != tok || f2 != fp {
			t.Fatalf("decoded %q %q %q does not encode back: %v", srv, tok, fp, err)
		}
	})
}
