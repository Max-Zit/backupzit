//go:build !windows

package agent

import (
	"bufio"
	"os"
	"runtime"
	"strings"
)

// osDescription returns PRETTY_NAME from /etc/os-release, e.g. "Ubuntu 24.04.1 LTS".
func osDescription() string {
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return runtime.GOOS
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "PRETTY_NAME="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return runtime.GOOS
}
