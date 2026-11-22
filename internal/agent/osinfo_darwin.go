package agent

import (
	"os/exec"
	"strings"
)

// osDescription returns e.g. "macOS 27.0.1 (26A434)" from sw_vers.
func osDescription() string {
	field := func(name string) string {
		out, err := exec.Command("/usr/bin/sw_vers", "-"+name).Output() // #nosec G204 -- fixed program, fixed option names
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	version := field("productVersion")
	if version == "" {
		return "macOS"
	}
	if build := field("buildVersion"); build != "" {
		return "macOS " + version + " (" + build + ")"
	}
	return "macOS " + version
}
