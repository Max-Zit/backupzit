package agent

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// The configuration of a Proxmox VE node (storage, network, users,
// firewall, guest settings) lives in an SQLite database that the cluster
// service writes while the node runs. A file backup reading it at the
// moment of a write could store a torn copy, so a backup whose paths
// include it also gets a consistent copy made just before.
var (
	pveConfigDB      = "/var/lib/pve-cluster/config.db"
	pveConfigCopyDir = "/var/lib/backupzit/pve-config"
	pveHostOS        = runtime.GOOS == "linux"
)

// withPVEConfigCopy adds a consistent copy of the node's configuration
// database to paths when they include the database.
func (a *Agent) withPVEConfigCopy(paths []string) []string {
	if !pveHostOS || !coversPath(paths, pveConfigDB) {
		return paths
	}
	if _, err := os.Stat(pveConfigDB); err != nil {
		return paths
	}
	if err := stableCopy(pveConfigCopyDir, pveConfigDB, pveConfigDB+"-wal"); err != nil {
		a.log.Warn("no consistent copy of the Proxmox configuration database; the live file is backed up", "err", err)
		return paths
	}
	if coversPath(paths, pveConfigCopyDir) {
		return paths
	}
	return append(append([]string(nil), paths...), pveConfigCopyDir)
}

// coversPath reports whether one of paths is p or a folder above it.
func coversPath(paths []string, p string) bool {
	for _, x := range paths {
		x = filepath.ToSlash(filepath.Clean(x))
		if x == p || x == "/" || strings.HasPrefix(p, strings.TrimSuffix(x, "/")+"/") {
			return true
		}
	}
	return false
}

// stableCopy copies the files (optional ones may be missing) into dir and
// repeats until none of them changed while being copied.
func stableCopy(dir string, files ...string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	type state struct {
		size int64
		mod  time.Time
		ok   bool
	}
	stat := func() []state {
		out := make([]state, len(files))
		for i, f := range files {
			if fi, err := os.Stat(f); err == nil {
				out[i] = state{fi.Size(), fi.ModTime(), true}
			}
		}
		return out
	}
	same := func(a, b []state) bool {
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	for try := 0; try < 20; try++ {
		if try > 0 {
			time.Sleep(250 * time.Millisecond)
		}
		before := stat()
		if !before[0].ok {
			return fmt.Errorf("%s not found", files[0])
		}
		var err error
		for i, f := range files {
			if before[i].ok {
				if err = copyFile(f, filepath.Join(dir, filepath.Base(f)+".tmp")); err != nil {
					break
				}
			}
		}
		if err != nil || !same(before, stat()) {
			continue // written meanwhile (or gone): again
		}
		for i, f := range files {
			dst := filepath.Join(dir, filepath.Base(f))
			if !before[i].ok {
				os.Remove(dst) // no stale write-ahead log next to the new copy
				continue
			}
			if err := os.Rename(dst+".tmp", dst); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("%s kept changing while it was copied", files[0])
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
