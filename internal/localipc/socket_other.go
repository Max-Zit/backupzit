//go:build !windows

package localipc

import (
	"context"
	"net"
	"os"
	"runtime"
)

// SocketPath is the agent's local socket (root only). macOS has no /run;
// its runtime folder is /var/run.
var SocketPath = map[bool]string{true: "/var/run/backupzit-agent.sock", false: "/run/backupzit-agent.sock"}[runtime.GOOS == "darwin"]

func Listen() (net.Listener, error) {
	_ = os.Remove(SocketPath)
	l, err := net.Listen("unix", SocketPath)
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(SocketPath, 0o600)
	return l, nil
}

func dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", SocketPath)
}
