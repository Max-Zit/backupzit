//go:build !windows

package localipc

import (
	"context"
	"net"
	"os"
)

// SocketPath is the agent's local socket (root only).
const SocketPath = "/run/backupzit-agent.sock"

func Listen() (net.Listener, error) {
	os.Remove(SocketPath)
	l, err := net.Listen("unix", SocketPath)
	if err != nil {
		return nil, err
	}
	os.Chmod(SocketPath, 0o600)
	return l, nil
}

func dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", SocketPath)
}
