package server

import "testing"

// A search result only counts for the storage that was searched.
func TestScanBase(t *testing.T) {
	local := Target{Kind: "local", URL: `D:\Backups`}
	usb := Target{Kind: "usb", URL: "usb://BZBACKUP*/BackupZit"}
	for _, c := range []struct {
		t    Target
		base string
		want bool
	}{
		{local, `D:\Backups`, true},
		{local, `C:\Windows`, false},
		{local, "", false},
		{usb, "usb://BZBACKUP2/BackupZit", true},
		{usb, "usb://bzbackup1/BackupZit/", true},
		{usb, "usb://OTHERDISK/BackupZit", false},
		{usb, "usb://BZBACKUP2/elsewhere", false},
		{usb, "sftp://evil/BackupZit", false},
		{usb, "usb://BZBACKUP2/BackupZit?x=1", false},
	} {
		if got := scanBase(c.t, c.base); got != c.want {
			t.Errorf("%s %q: %v", c.t.Kind, c.base, got)
		}
	}
}
