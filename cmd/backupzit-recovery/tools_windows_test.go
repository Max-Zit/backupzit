//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A program started in its own console window must write to and read
// from that console, not from NUL: otherwise the window stays empty and a
// command prompt closes at once (stdin at its end).
func TestStartDetachedGetsARealConsole(t *testing.T) {
	out := filepath.Join(t.TempDir(), "redirected.txt")
	startDetached("powershell.exe", "-NoProfile", "-Command",
		"Set-Content -LiteralPath '"+out+"' -Value ([Console]::IsOutputRedirected.ToString() + ' ' + [Console]::IsInputRedirected.ToString())")
	var got []byte
	for i := 0; i < 100; i++ {
		if b, err := os.ReadFile(out); err == nil && len(b) > 0 {
			got = b
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if s := strings.TrimSpace(string(got)); s != "False False" {
		t.Fatalf("output/input redirected: %q, want \"False False\"", s)
	}
}
