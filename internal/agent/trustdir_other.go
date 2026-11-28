//go:build !windows

package agent

// trustDir: on Linux and macOS the agent's folders are below /etc,
// /var/lib and /Library, where only root can create anything.
func trustDir(string) (string, error) { return "", nil }
