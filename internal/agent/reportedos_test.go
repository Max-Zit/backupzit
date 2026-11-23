package agent

import "testing"

// BACKUPZIT_REPORT_OS changes what a Mac reports, and nothing on Windows
// and Linux, where the reported system decides which jobs and updates the
// console offers.
func TestReportedOS(t *testing.T) {
	for _, c := range []struct{ goos, real, override, want string }{
		{"darwin", "macOS 27.0.1 (26A434)", "", "macOS 27.0.1 (26A434)"},
		{"darwin", "macOS 27.0.1 (26A434)", " Debian GNU/Linux 12 ", "Debian GNU/Linux 12"},
		{"linux", "Ubuntu 24.04.1 LTS", "Windows 11", "Ubuntu 24.04.1 LTS"},
		{"windows", "Windows 11 Pro (build 26100)", "Debian GNU/Linux 12", "Windows 11 Pro (build 26100)"},
	} {
		if got := reportedOS(c.goos, c.real, c.override); got != c.want {
			t.Errorf("%s %q %q: %q, want %q", c.goos, c.real, c.override, got, c.want)
		}
	}
}
