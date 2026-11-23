//go:build !windows && !darwin

package localipc

import (
	"context"
	"net"
	"os"
)

// SocketPath is the agent's local socket (root only).
var SocketPath = "/run/backupzit-agent.sock"

func Listen() (net.Listener, error) {
	_ = os.Remove(SocketPath)
	l, err := net.Listen("unix", SocketPath)
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(SocketPath, 0o600)
	return l, nil
}

// authorize lets every caller through: the socket is 0600 root, so only
// root can connect.
func authorize(net.Conn, string) error { return nil }

func dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", SocketPath)
}
