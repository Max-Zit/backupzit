//go:build !linux

package pve

import "os"

// mountedDir: Proxmox VE runs on Linux; elsewhere (tests) a folder counts.
var mountedDir = func(dir string) bool {
	fi, err := os.Stat(dir)
	return err == nil && fi.IsDir()
}
