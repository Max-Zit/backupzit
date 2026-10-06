package server

import (
	"strings"
	"testing"
)

func TestSSORoles(t *testing.T) {
	c := SSOSettings{AdminGroup: "BZ-Admins", ViewerGroup: "bz-viewers"}
	for _, x := range []struct {
		groups []string
		want   string
	}{
		{[]string{"other", "bz-admins"}, "admin"},      // case-insensitive
		{[]string{"bz-viewers", "BZ-Admins"}, "admin"}, // highest wins
		{[]string{"bz-viewers"}, "viewer"},
		{nil, ""},
	} {
		got, err := c.role(x.groups)
		if got != x.want || (x.want == "" && err == nil) {
			t.Errorf("%v: %q %v, want %q", x.groups, got, err, x.want)
		}
	}
	c.DefaultRole = "restore"
	if got, _ := c.role([]string{"nothing"}); got != "restore" {
		t.Errorf("default role: %q", got)
	}
}

func TestSSOValidate(t *testing.T) {
	for _, x := range []struct {
		c  SSOSettings
		ok bool
	}{
		{SSOSettings{}, true},
		{SSOSettings{Protocol: "oidc", Issuer: "https://idp.example/realms/a", ClientID: "c", ClientSecret: "s", AdminGroup: "g"}, true},
		{SSOSettings{Protocol: "oidc", Issuer: "http://idp.example", ClientID: "c", ClientSecret: "s", AdminGroup: "g"}, false},
		{SSOSettings{Protocol: "oidc", Issuer: "https://idp.example", ClientID: "c", AdminGroup: "g"}, false},
		{SSOSettings{Protocol: "saml", MetadataURL: "https://idp.example/md", DefaultRole: "viewer"}, true},
		{SSOSettings{Protocol: "saml", AdminGroup: "g"}, false},
		{SSOSettings{Protocol: "saml", MetadataURL: "https://idp.example/md"}, false}, // no role at all
		{SSOSettings{Protocol: "ldap"}, false},
	} {
		c := x.c
		if err := c.Validate(); (err == nil) != x.ok {
			t.Errorf("%+v: %v", x.c, err)
		}
	}
}

func TestSSOState(t *testing.T) {
	tok := ssoState("state1", "nonce1")
	v, ok := parseSSOState(tok)
	if !ok || len(v) != 2 || v[0] != "state1" || v[1] != "nonce1" {
		t.Fatalf("round trip: %v %v", v, ok)
	}
	payload, sig, _ := strings.Cut(tok, ".")
	if _, ok := parseSSOState(payload + "x." + sig); ok {
		t.Error("changed state accepted")
	}
	if !firstUse("t:1") || firstUse("t:1") {
		t.Error("a state can be used once only")
	}
	if got := stringList([]any{"a", 1, "b"}); len(got) != 2 {
		t.Errorf("stringList: %v", got)
	}
}
