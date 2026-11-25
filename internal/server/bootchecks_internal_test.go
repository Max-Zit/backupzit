package server

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Boot test checks are validated with the job, and only users who may
// change commands set them; others keep the job's checks (0.61).
func TestJobBootChecks(t *testing.T) {
	if err := (JobOptions{BootChecks: []string{"service sshd", "101: port 443"}}).Validate(); err != nil {
		t.Errorf("good checks refused: %v", err)
	}
	if err := (JobOptions{BootChecks: []string{"ping 10.0.0.1"}}).Validate(); err == nil {
		t.Error("unknown check accepted")
	}
	req := httptest.NewRequest("POST", "/jobs/1", strings.NewReader(url.Values{"boot_checks": {"service nginx\r\nfile /etc/x"}, "boot_oldest": {"on"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.ParseForm()
	old := JobOptions{BootChecks: []string{"port 22"}}
	if o := optionsFromForm(req, old, true); len(o.BootChecks) != 2 || o.BootChecks[1] != "file /etc/x" || !o.BootOldest {
		t.Errorf("admin: %+v %v", o.BootChecks, o.BootOldest)
	}
	if o := optionsFromForm(req, old, false); len(o.BootChecks) != 1 || o.BootChecks[0] != "port 22" {
		t.Errorf("user without commands changed the checks: %+v", o.BootChecks)
	}
}
