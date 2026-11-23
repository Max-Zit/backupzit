package localipc

import "slices"

// peer is the identity of a process connected to the socket.
type peer struct {
	uid    uint32
	groups []uint32 // effective group first, as the kernel reports them
}

// userCommands are what a signed-in user may ask, as the Windows pipe lets
// interactive users read the status and start backups (GRGW).
var userCommands = []string{CmdStatus, CmdRun}

// allowedCommand decides whether p may run cmd: root (the agent's own
// tools, "sudo backupzit-agent status") may run everything, like SYSTEM and
// administrators on Windows; a member of staff (a person's account) may run
// the user commands; any other account (daemons) nothing.
func allowedCommand(p peer, cmd string, staffGID uint32) bool {
	if p.uid == 0 {
		return true
	}
	if !slices.Contains(p.groups, staffGID) {
		return false
	}
	return slices.Contains(userCommands, cmd)
}
