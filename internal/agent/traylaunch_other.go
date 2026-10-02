//go:build !windows

package agent

import "log/slog"

// StartTrayInSessions is Windows only.
func StartTrayInSessions(*slog.Logger) {}
