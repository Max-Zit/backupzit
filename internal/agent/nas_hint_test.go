package agent

import (
	"errors"
	"strings"
	"testing"
)

func TestNASHint(t *testing.T) {
	for msg, want := range map[string]string{
		"mount: mount error(2): No such file or directory":              "does not exist",
		"mount: mount error(13): Permission denied":                     "access denied",
		"mount.nfs: access denied by server while mounting x:/y":        "NFS export",
		"System error 53 has occurred. The network path was not found.": "does not answer",
		"something else": "",
	} {
		if got := nasHint(errors.New(msg)); (want == "" && got != "") || !strings.Contains(got, want) {
			t.Errorf("%q: %q", msg, got)
		}
	}
}

func TestFailedFullStorageHint(t *testing.T) {
	r := failed(errors.New(`upload pack x: sftp write /a: sftp: "Failure" (SSH_FX_FAILURE)`))
	if !strings.Contains(r.Message, "storage may be full") {
		t.Error(r.Message)
	}
	if r := failed(errors.New("other")); r.Message != "other" {
		t.Error(r.Message)
	}
}
