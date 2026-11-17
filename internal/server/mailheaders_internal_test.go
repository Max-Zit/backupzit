package server

import (
	"mime"
	"net/mail"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/api"
)

// Run alerts carry language-independent headers for mail filters (issue
// #33). The host name comes from the agent and must never be able to add
// headers of its own.
func TestRunHeaders(t *testing.T) {
	job := "Nightly – Büro"
	r := Run{ID: 42, Kind: api.KindBackup, Status: api.StatusSuccess, JobName: &job,
		Hostname: "srv01\r\nBcc: evil@example.com\r\nX-Evil: 1", Anomaly: "many files changed"}
	var hdr strings.Builder
	for _, h := range runHeaders(r) {
		hdr.WriteString(headerLine(h))
	}
	msg, err := mail.ReadMessage(strings.NewReader(hdr.String() + "\r\nbody"))
	if err != nil {
		t.Fatal(err)
	}
	if msg.Header.Get("Bcc") != "" || msg.Header.Get("X-Evil") != "" {
		t.Fatalf("header injection: %q", hdr.String())
	}
	dec := mimeDecoder{}
	for name, want := range map[string]string{
		"X-BackupZit-Status":     "success",
		"X-BackupZit-Kind":       "backup",
		"X-BackupZit-Run":        "42",
		"X-BackupZit-Job":        job,
		"X-BackupZit-Host":       "srv01\r\nBcc: evil@example.com\r\nX-Evil: 1",
		"X-BackupZit-Suspicious": "yes",
	} {
		if got := dec.decode(msg.Header.Get(name)); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}

	// Long values are cut, never split into extra lines of their own.
	long := strings.Repeat("ž", 500)
	line := headerLine(mailHeader{"X-BackupZit-Job", long})
	m2, err := mail.ReadMessage(strings.NewReader(line + "\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := dec.decode(m2.Header.Get("X-BackupZit-Job")); got != strings.Repeat("ž", maxHeaderValue) {
		t.Errorf("long value: %d runes", len([]rune(got)))
	}

	// Without a job or an anomaly those headers are left out.
	r.JobName, r.Anomaly = nil, ""
	for _, h := range runHeaders(r) {
		if h.Name == "X-BackupZit-Job" || h.Name == "X-BackupZit-Suspicious" {
			t.Errorf("unexpected %s", h.Name)
		}
	}
}

type mimeDecoder struct{}

func (mimeDecoder) decode(s string) string {
	out, err := new(mime.WordDecoder).DecodeHeader(s)
	if err != nil {
		return "decode error: " + err.Error()
	}
	return out
}
