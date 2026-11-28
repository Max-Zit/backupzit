package main

import (
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/api"
)

// MED 15a: "enroll --code -" reads the code from the input, so a script
// (the console's push-install) need not put it on the command line.
func TestReadEnrollCode(t *testing.T) {
	code := api.EncodeEnrollCode("https://console:8443", "token-1", "SHA256:abc")
	got, err := readEnrollCode(strings.NewReader(code + "\r\n"))
	if err != nil || got != code {
		t.Fatalf("read %q, %v", got, err)
	}
	if s, tok, fp, err := api.DecodeEnrollCode(got); err != nil || s != "https://console:8443" || tok != "token-1" || fp != "SHA256:abc" {
		t.Fatalf("decode: %s %s %s %v", s, tok, fp, err)
	}
	if _, err := readEnrollCode(strings.NewReader(" \n")); err == nil {
		t.Error("empty input accepted")
	}
}
