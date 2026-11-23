package agent

import (
	"os"
	"path/filepath"

	"github.com/max-zit/backupzit/internal/localipc"
	"github.com/max-zit/backupzit/internal/macperm"
)

// MacPermissions are the macOS permissions of this agent process for the
// menu bar app, nil on other systems (so their status JSON is unchanged).
// It is checked on every status request: the user may grant or remove
// Full Disk Access at any time, and the check is two file opens.
func MacPermissions() *localipc.MacPermissions {
	fda := macperm.FullDiskAccess()
	if fda == macperm.NotApplicable {
		return nil
	}
	return &localipc.MacPermissions{FullDiskAccess: string(fda), AgentPath: agentProgram()}
}

// agentProgram is the real file of this program: macOS gives Full Disk
// Access to a file, not to a link pointing at it.
func agentProgram() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		return real
	}
	return exe
}
