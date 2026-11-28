package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestConsoleURLs(t *testing.T) {
	addrs := []string{"192.168.1.20/24", "10.0.0.5/8"}
	pkg, err := os.ReadFile(filepath.Join("..", "..", "packaging", "linux", "server.env"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, env string
		want      []string
	}{
		{"server.env of the package", string(pkg), []string{"https://192.168.1.20:8443", "https://10.0.0.5:8443"}},
		{"no settings", "", []string{"https://192.168.1.20:8443", "https://10.0.0.5:8443"}},
		{"port 3000", "BACKUPZIT_LISTEN=:3000\n", []string{"https://192.168.1.20:3000", "https://10.0.0.5:3000"}},
		{"address and port, quoted, CRLF", "BACKUPZIT_LISTEN=\"0.0.0.0:3000\"\r\n", []string{"https://192.168.1.20:3000", "https://10.0.0.5:3000"}},
		{"port 443 is not shown", "BACKUPZIT_LISTEN=:443\n", []string{"https://192.168.1.20", "https://10.0.0.5"}},
		{"the last line counts", "BACKUPZIT_LISTEN=:3000\nBACKUPZIT_LISTEN=:4000\n", []string{"https://192.168.1.20:4000", "https://10.0.0.5:4000"}},
		{"commented out", "#BACKUPZIT_LISTEN=:3000\n", []string{"https://192.168.1.20:8443", "https://10.0.0.5:8443"}},
		{"not a port", "BACKUPZIT_LISTEN=:70000\n", []string{"https://192.168.1.20:8443", "https://10.0.0.5:8443"}},
		{"no colon", "BACKUPZIT_LISTEN=3000\n", []string{"https://192.168.1.20:8443", "https://10.0.0.5:8443"}},
		{"public URL", "BACKUPZIT_LISTEN=:3000\nBACKUPZIT_PUBLIC_URL=https://backup.example.com:3000\n", []string{"https://backup.example.com:3000"}},
		{"public URL empty", "BACKUPZIT_PUBLIC_URL=\nBACKUPZIT_LISTEN=:3000\n", []string{"https://192.168.1.20:3000", "https://10.0.0.5:3000"}},
	} {
		if got := consoleURLs(parseEnv(c.env), addrs); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: consoleURLs = %q, want %q", c.name, got, c.want)
		}
	}
}
