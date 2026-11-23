package sysbackup

import (
	"bufio"
	"os"
	"strings"
)

// selinuxActive reports whether an SELinux config file (etc/selinux/config)
// turns SELinux on (enforcing or permissive).
func selinuxActive(config string) bool {
	f, err := os.Open(config) // #nosec G304 -- a fixed path inside the restored system
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if ok && strings.TrimSpace(k) == "SELINUX" {
			v = strings.ToLower(strings.Trim(strings.TrimSpace(v), `"'`))
			return v == "enforcing" || v == "permissive"
		}
	}
	return false
}
