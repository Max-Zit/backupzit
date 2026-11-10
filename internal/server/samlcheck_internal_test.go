package server

import (
	"os"
	"strings"
	"testing"

	"github.com/crewjam/saml"
)

const samlOK = `<samlp:Response xmlns:samlp="urn:oasis:names:tc:SAML:2.0:protocol" xmlns:saml="urn:oasis:names:tc:SAML:2.0:assertion">
<saml:Issuer>https://idp</saml:Issuer><samlp:Status><samlp:StatusCode Value="urn:oasis:names:tc:SAML:2.0:status:Success"/></samlp:Status>
<saml:Assertion ID="a"><saml:Issuer>https://idp</saml:Issuer>
<ds:Signature xmlns:ds="http://www.w3.org/2000/09/xmldsig#"><ds:SignedInfo/></ds:Signature>
<saml:Subject><saml:NameID>alice</saml:NameID><saml:SubjectConfirmation Method="urn:oasis:names:tc:SAML:2.0:cm:bearer"><saml:SubjectConfirmationData/></saml:SubjectConfirmation></saml:Subject>
<saml:AttributeStatement><saml:Attribute Name="groups"><saml:AttributeValue xmlns:xs="http://www.w3.org/2001/XMLSchema">admins</saml:AttributeValue></saml:Attribute></saml:AttributeStatement>
</saml:Assertion></samlp:Response>`

// GO-2026-6909: a NameID (or another bound element) from a foreign
// namespace, or an assertion without a subject, is refused before the
// library reads it; normal responses pass.
func TestCheckSAMLXML(t *testing.T) {
	if err := checkSAMLXML([]byte(samlOK)); err != nil {
		t.Fatalf("normal response refused: %v", err)
	}
	for name, bad := range map[string]string{
		"foreign NameID":    strings.Replace(samlOK, `<saml:NameID>alice</saml:NameID>`, `<x:NameID xmlns:x="urn:evil">admin</x:NameID>`, 1),
		"default-ns NameID": strings.Replace(samlOK, `<saml:NameID>alice</saml:NameID>`, `<NameID xmlns="">admin</NameID>`, 1),
		"foreign Attribute": strings.Replace(samlOK, `<saml:Attribute Name="groups">`, `<x:Attribute xmlns:x="urn:evil" Name="groups">`, 1),
		"no issuer":         strings.Replace(samlOK, `<saml:Assertion ID="a"><saml:Issuer>https://idp</saml:Issuer>`, `<saml:Assertion ID="a">`, 1),
		"no subject":        strings.Replace(samlOK, `<saml:Subject><saml:NameID>alice</saml:NameID><saml:SubjectConfirmation Method="urn:oasis:names:tc:SAML:2.0:cm:bearer"><saml:SubjectConfirmationData/></saml:SubjectConfirmation></saml:Subject>`, ``, 1),
		"not xml":           `<saml:Assertion`,
	} {
		if name == "foreign Attribute" {
			bad = strings.Replace(bad, `</saml:Attribute></saml:AttributeStatement>`, `</x:Attribute></saml:AttributeStatement>`, 1)
		}
		if err := checkSAMLXML([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := checkSAMLResponse([]byte(samlOK), nil); err != nil {
		t.Fatalf("checkSAMLResponse: %v", err)
	}
	enc := strings.Replace(samlOK, `</samlp:Response>`, `<saml:EncryptedAssertion><xenc:EncryptedData xmlns:xenc="http://www.w3.org/2001/04/xmlenc#"/></saml:EncryptedAssertion></samlp:Response>`, 1)
	if err := checkSAMLResponse([]byte(enc), nil); err == nil {
		t.Error("an encrypted assertion that does not decrypt was accepted")
	}
}

// A panic in the library's parser is a refused response.
func TestParseSAMLResponseRecovers(t *testing.T) {
	if _, err := parseSAMLResponse(func() (*saml.Assertion, error) { var a *saml.Assertion; _ = a.Subject.NameID; return a, nil }); err == nil {
		t.Fatal("panic not turned into an error")
	}
}

// Whatever comes in a SAMLResponse: no panic in the check.
func FuzzCheckSAMLResponse(f *testing.F) {
	f.Add([]byte(samlOK))
	if b, err := os.ReadFile("testdata/saml-keycloak-response.xml"); err == nil {
		f.Add(b)
	}
	f.Add([]byte(`<saml:EncryptedAssertion xmlns:saml="urn:oasis:names:tc:SAML:2.0:assertion"><EncryptedData/><EncryptedKey/></saml:EncryptedAssertion>`))
	f.Add([]byte(`<a><b></a>`))
	f.Fuzz(func(t *testing.T, b []byte) {
		_ = checkSAMLResponse(b, nil)
	})
}

// A real response of Keycloak 26 (realm rztest in the lab, signed
// assertion) passes the check; the same with a foreign NameID does not.
func TestCheckSAMLKeycloakResponse(t *testing.T) {
	raw, err := os.ReadFile("testdata/saml-keycloak-response.xml")
	if err != nil {
		t.Fatal(err)
	}
	if err := checkSAMLResponse(raw, nil); err != nil {
		t.Fatalf("Keycloak response refused: %v", err)
	}
	bad := strings.Replace(string(raw), "<saml:NameID", `<x:NameID xmlns:x="urn:evil"`, 1)
	bad = strings.Replace(bad, "</saml:NameID>", "</x:NameID>", 1)
	if checkSAMLResponse([]byte(bad), nil) == nil {
		t.Fatal("foreign NameID accepted")
	}
}
