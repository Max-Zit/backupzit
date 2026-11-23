package hyperv

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// A quoted value comes back from PowerShell unchanged and never runs as a
// command, also with the typographic single quotes PowerShell treats like '
// (a VM name typed in the console, or one from a backup).
func TestPsqKeepsValuesLiteral(t *testing.T) {
	if _, err := exec.LookPath("powershell.exe"); err != nil {
		t.Skip("no PowerShell")
	}
	for _, v := range []string{
		"plain", "it's", "a'; Write-Output INJECTED; 'b",
		"a’; Write-Output INJECTED; ’b", "a‘; Write-Output INJECTED; ‘b",
		"a‚; Write-Output INJECTED; ‛b", "‘’‚‛''",
		"$(Write-Output INJECTED)", "`$x `n \"q\"", "Žućko 日本",
	} {
		out, err := ps(context.Background(), `Write-Output `+psq(v))
		if err != nil {
			t.Fatalf("%q: %v", v, err)
		}
		got := strings.TrimRight(string(out), "\r\n")
		if got != v {
			t.Errorf("psq(%q) came back as %q", v, got)
		}
	}
}
