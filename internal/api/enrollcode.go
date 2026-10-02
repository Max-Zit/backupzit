package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

// An enrollment code carries everything an agent needs to join a console —
// server URL, enrollment token and the pinned certificate fingerprint — in
// one string that can be pasted into the installer: "BZ1-<base64url JSON>".
const enrollCodePrefix = "BZ1-"

type enrollCode struct {
	Server      string `json:"s"`
	Token       string `json:"t"`
	Fingerprint string `json:"f"`
}

// EncodeEnrollCode builds an enrollment code.
func EncodeEnrollCode(server, token, fingerprint string) string {
	b, _ := json.Marshal(enrollCode{server, token, fingerprint})
	return enrollCodePrefix + base64.RawURLEncoding.EncodeToString(b)
}

// DecodeEnrollCode parses an enrollment code.
func DecodeEnrollCode(code string) (server, token, fingerprint string, err error) {
	code = strings.Join(strings.Fields(code), "") // tolerate line breaks from copying
	if !strings.HasPrefix(code, enrollCodePrefix) {
		return "", "", "", errors.New("not a BackupZit enrollment code (it starts with BZ1-)")
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(code, enrollCodePrefix))
	if err != nil {
		return "", "", "", errors.New("the enrollment code is incomplete — copy it again from the console")
	}
	var c enrollCode
	if err := json.Unmarshal(b, &c); err != nil || c.Server == "" || c.Token == "" || !strings.HasPrefix(c.Fingerprint, "SHA256:") {
		return "", "", "", errors.New("the enrollment code is damaged — copy it again from the console")
	}
	return c.Server, c.Token, c.Fingerprint, nil
}
