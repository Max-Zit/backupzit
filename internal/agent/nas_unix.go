//go:build !windows

package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/max-zit/backupzit/internal/api"
)

// nasMountDir holds the mount points of NAS shares, one per share, so the
// paths in the snapshots stay the same from backup to backup.
const nasMountDir = "/mnt/backupzit-nas"

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func mountNAS(ctx context.Context, s nasShare, cred *api.NASShare, writable bool) (string, func(), error) {
	helper := map[string]string{"smb": "mount.cifs", "nfs": "mount.nfs"}[s.Proto]
	if _, err := exec.LookPath(helper); err != nil {
		pkg := map[string]string{"smb": "cifs-utils", "nfs": "nfs-common (Debian, Ubuntu) or nfs-utils (AlmaLinux, Rocky Linux)"}[s.Proto]
		return "", nil, fmt.Errorf("%s is missing on this machine; install %s", helper, pkg)
	}
	dir := filepath.Join(nasMountDir, unsafeName.ReplaceAllString(s.Host, "_"),
		unsafeName.ReplaceAllString(strings.Trim(s.Share, "/"), "_"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}
	if mounted(dir) {
		exec.CommandContext(ctx, "umount", dir).Run() // left over from an interrupted run
	}
	mode := "ro"
	if writable {
		mode = "rw"
	}
	var args []string
	switch s.Proto {
	case "smb":
		opts := mode + ",iocharset=utf8"
		if cred.User == "" {
			opts += ",guest"
		} else {
			// Credentials go through a file only root can read, not the
			// command line.
			f, err := os.CreateTemp("", "bz-nas-*.cred")
			if err != nil {
				return "", nil, err
			}
			defer os.Remove(f.Name())
			fmt.Fprintf(f, "username=%s\npassword=%s\n", cred.User, cred.Password)
			if cred.Domain != "" {
				fmt.Fprintf(f, "domain=%s\n", cred.Domain)
			}
			f.Close()
			opts += ",credentials=" + f.Name()
		}
		args = []string{"-t", "cifs", "//" + s.Host + "/" + s.Share, dir, "-o", opts}
	case "nfs":
		args = []string{"-t", "nfs", s.Host + ":" + s.Share, dir, "-o", mode + ",nolock"}
	}
	if out, err := exec.CommandContext(ctx, "mount", args...).CombinedOutput(); err != nil {
		return "", nil, fmt.Errorf("mount: %s", strings.TrimSpace(string(out)))
	}
	unmount := func() {
		if err := exec.Command("umount", dir).Run(); err != nil {
			exec.Command("umount", "-l", dir).Run()
		}
	}
	root := dir
	if s.Sub != "" {
		root = filepath.Join(dir, filepath.FromSlash(s.Sub))
		if _, err := os.Stat(root); err != nil {
			unmount()
			return "", nil, errors.New("the folder " + s.Sub + " does not exist on the share")
		}
	}
	return root, unmount, nil
}

// mounted reports whether dir is a mount point.
func mounted(dir string) bool {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if fields := strings.Fields(sc.Text()); len(fields) > 4 && unescapeMountinfo(fields[4]) == dir {
			return true
		}
	}
	return false
}

func unescapeMountinfo(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}
