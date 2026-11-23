package localipc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"strconv"

	"golang.org/x/sys/unix"
)

// SocketPath is the agent's local socket. macOS has no /run; its runtime
// folder is /var/run.
var SocketPath = "/var/run/backupzit-agent.sock"

// staffGroup is the group of every local account a person signs in with
// (and of no system daemon account): the macOS counterpart of the
// "interactive users" the Windows pipe ACL lets in.
const staffGroup = "staff"

// defaultStaffGID is staff's group id on every macOS release, used should
// the directory service not answer.
const defaultStaffGID = 20

// Listen creates the socket for the agent (running as root) and the menu
// bar app of each signed-in user (running as that user).
//
// The Windows pipe gives SYSTEM and administrators full access and lets
// interactively logged-on users read the status and start backups. macOS
// mirrors that in two layers:
//   - the socket is root:staff 0660, so only root and members of staff can
//     connect at all (daemon accounts such as _www cannot);
//   - every request is checked against the caller's credentials, which the
//     kernel records for the connected peer (LOCAL_PEERCRED) and the caller
//     cannot forge: see authorize and allowedCommand.
//
// Between the bind and the chmod the socket briefly has the umask's mode
// (0755 for a root daemon): connecting needs write permission, so nobody
// but root can connect in that window either.
func Listen() (net.Listener, error) {
	_ = os.Remove(SocketPath)
	l, err := net.Listen("unix", SocketPath)
	if err != nil {
		return nil, err
	}
	// Failing to give the group access only keeps the socket root-only (the
	// menu bar app then shows the agent as unreachable); backups still run.
	// Owner and mode are set without following a link: /var/run is root's,
	// so nobody else can put one at the name, but should that ever change
	// a link would hand another file to the staff group.
	if err := unix.Fchownat(unix.AT_FDCWD, SocketPath, 0, int(staffGID()), unix.AT_SYMLINK_NOFOLLOW); err != nil {
		_ = unix.Fchmodat(unix.AT_FDCWD, SocketPath, 0o600, unix.AT_SYMLINK_NOFOLLOW)
		return l, nil
	}
	_ = unix.Fchmodat(unix.AT_FDCWD, SocketPath, 0o660, unix.AT_SYMLINK_NOFOLLOW) // staff are the callers allowed in; every request is checked by authorize
	return l, nil
}

// staffGID looks up the staff group's id.
func staffGID() uint32 {
	if g, err := user.LookupGroup(staffGroup); err == nil {
		if id, err := strconv.ParseUint(g.Gid, 10, 32); err == nil {
			return uint32(id)
		}
	}
	return defaultStaffGID
}

// authorize checks that the process at the other end of c may run cmd.
func authorize(c net.Conn, cmd string) error {
	p, err := peerOf(c)
	if err != nil {
		return fmt.Errorf("cannot identify the caller: %w", err)
	}
	if !allowedCommand(p, cmd, staffGID()) {
		return errors.New("not allowed for this user")
	}
	return nil
}

// peerOf returns the credentials of the process connected to c, as the
// kernel recorded them when it connected.
func peerOf(c net.Conn) (peer, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return peer{}, errors.New("not a unix socket")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return peer{}, err
	}
	var cred *unix.Xucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED) // #nosec G115 -- a file descriptor fits an int
	}); err != nil {
		return peer{}, err
	}
	if credErr != nil {
		return peer{}, credErr
	}
	return peerFromXucred(cred), nil
}

// peerFromXucred keeps the groups the kernel filled in.
func peerFromXucred(cred *unix.Xucred) peer {
	n := int(cred.Ngroups)
	if n < 0 || n > len(cred.Groups) {
		n = len(cred.Groups)
	}
	return peer{uid: cred.Uid, groups: append([]uint32(nil), cred.Groups[:n]...)}
}

func dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", SocketPath)
}
