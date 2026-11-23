package server

import (
	"bytes"
	"crypto"
	"encoding/xml"
	"errors"
	"fmt"
	"io"

	"github.com/beevik/etree"
	"github.com/crewjam/saml"
	"github.com/crewjam/saml/xmlenc"
)

// GO-2026-6909: crewjam/saml (v0.6.3 too: the vulnerability database lists
// no fixed release) binds some assertion elements
// without their namespace (a NameID of any namespace fills Subject.NameID,
// so it could be spoofed inside a signed assertion) and dereferences an
// optional Subject without a check (a panic). Until a fixed release is
// listed, the console checks every SAML response itself before the library
// reads it: these elements must be in the SAML assertion namespace, and
// every assertion needs a Subject. Encrypted assertions are decrypted and
// checked the same way.

const samlAssertionNS = "urn:oasis:names:tc:SAML:2.0:assertion"

// samlBound are the assertion elements the library maps by local name.
var samlBound = map[string]bool{
	"Assertion": true, "Issuer": true, "Subject": true, "NameID": true, "Conditions": true,
	"AudienceRestriction": true, "Audience": true, "AuthnStatement": true, "AuthnContext": true,
	"AuthnContextClassRef": true, "AttributeStatement": true, "Attribute": true, "AttributeValue": true,
	"SubjectConfirmation": true, "SubjectConfirmationData": true, "OneTimeUse": true, "ProxyRestriction": true,
}

// samlErr is an error of the SAML check. Only the log shows it (the user sees
// "The response of the identity provider is not valid."), so it is not
// translated.
type samlErr string

func (e samlErr) Error() string { return string(e) }

const errSAMLShape = samlErr("SAML response with assertion elements outside the SAML namespace or without an issuer or a subject")

// checkSAMLXML checks one XML document (a response or a decrypted assertion).
func checkSAMLXML(b []byte) error {
	d := xml.NewDecoder(bytes.NewReader(b))
	type open struct {
		name       xml.Name
		hasSubject bool
		hasIssuer  bool
	}
	var stack []open
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("SAML response is not valid XML: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if samlBound[t.Name.Local] && t.Name.Space != samlAssertionNS {
				return errSAMLShape
			}
			if t.Name.Local == "Subject" && len(stack) > 0 && stack[len(stack)-1].name.Local == "Assertion" {
				stack[len(stack)-1].hasSubject = true
			}
			if t.Name.Local == "Issuer" && len(stack) > 0 && stack[len(stack)-1].name.Local == "Assertion" {
				stack[len(stack)-1].hasIssuer = true
			}
			stack = append(stack, open{name: t.Name})
		case xml.EndElement:
			if len(stack) == 0 {
				return errSAMLShape
			}
			top := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if top.name.Local == "Assertion" && top.name.Space == samlAssertionNS && (!top.hasSubject || !top.hasIssuer) {
				return errSAMLShape
			}
		}
	}
}

// checkSAMLResponse checks a decoded SAMLResponse, and the plaintext of
// every encrypted assertion in it (decrypted with the console's SAML key,
// as the library does).
func checkSAMLResponse(raw []byte, key crypto.Signer) error {
	if err := checkSAMLXML(raw); err != nil {
		return err
	}
	return checkEncrypted(raw, key, 0)
}

// checkEncrypted decrypts every encrypted assertion and encrypted NameID
// (EncryptedID, which the library decrypts into the subject's NameID: it
// went unchecked, L-N1) in the XML b and checks the plaintext, also the
// encrypted parts inside a decrypted assertion.
func checkEncrypted(b []byte, key crypto.Signer, depth int) error {
	if depth > 2 {
		return errSAMLShape
	}
	doc := etree.NewDocument()
	if err := doc.ReadFromBytes(b); err != nil {
		return fmt.Errorf("SAML response is not valid XML: %w", err)
	}
	var encs []*etree.Element
	encs = append(encs, doc.FindElements("//EncryptedAssertion")...)
	encs = append(encs, doc.FindElements("//EncryptedID")...)
	for _, enc := range encs {
		data := enc.FindElement("./EncryptedData")
		if data == nil {
			return errSAMLShape
		}
		var k any = key
		if ke := enc.FindElement("./EncryptedKey"); ke != nil {
			var err error
			if k, err = xmlenc.Decrypt(key, ke); err != nil {
				return fmt.Errorf("SAML key: %w", err)
			}
		}
		plain, err := xmlenc.Decrypt(k, data)
		if err != nil {
			return fmt.Errorf("SAML %s: %w", enc.Tag, err)
		}
		if err := checkSAMLXML(plain); err != nil {
			return err
		}
		if enc.Tag == "EncryptedAssertion" {
			if err := checkEncrypted(plain, key, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// parseSAMLResponse runs the library's parser; a panic in it (an answer
// the checks above did not foresee) is a refused response, not a crash of
// the request.
func parseSAMLResponse(parse func() (*saml.Assertion, error)) (a *saml.Assertion, err error) {
	defer func() {
		if p := recover(); p != nil {
			a, err = nil, fmt.Errorf("SAML response made the parser fail: %v", p)
		}
	}()
	return parse()
}
