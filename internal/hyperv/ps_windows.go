package hyperv

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
)

// ps runs a PowerShell script (Hyper-V module) and returns its output.
var ps = func(ctx context.Context, script string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command",
		"$ErrorActionPreference='Stop'; $ProgressPreference='SilentlyContinue'; "+script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if i := strings.Index(msg, "\n"); i > 0 {
			msg = msg[:i]
		}
		return out, fmt.Errorf("%w: %s", err, msg)
	}
	return out, nil
}

// psq quotes a value for a single-quoted PowerShell string.
func psq(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
