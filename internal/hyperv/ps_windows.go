package hyperv

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"syscall"

	"github.com/max-zit/backupzit/internal/winenc"
)

// ps runs a PowerShell script (Hyper-V module) and returns its output. The
// output is UTF-8: in the console code page, VM names and paths with e.g.
// Japanese letters would be garbled or lost.
var ps = func(ctx context.Context, script string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command",
		"[Console]::OutputEncoding=New-Object System.Text.UTF8Encoding $false; $ErrorActionPreference='Stop'; $ProgressPreference='SilentlyContinue'; "+script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(winenc.Text(stderr.Bytes()))
		if i := strings.Index(msg, "\n"); i > 0 {
			msg = msg[:i]
		}
		return []byte(winenc.Text(out)), fmt.Errorf("%w: %s", err, msg)
	}
	return []byte(winenc.Text(out)), nil
}

// psq quotes a value for a single-quoted PowerShell string.
func psq(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
