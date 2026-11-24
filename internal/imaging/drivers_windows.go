//go:build windows

package imaging

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strings"
)

// LoadDrivers loads every driver (.inf) below dir into the running Windows
// PE with drvload, e.g. a storage driver for disks that are not visible.
// Outside Windows PE drvload is not available.
func LoadDrivers(ctx context.Context, dir string, log func(string)) (*LoadDriversResult, error) {
	if log == nil {
		log = func(string) {}
	}
	exe, err := exec.LookPath("drvload.exe")
	if err != nil {
		return nil, errors.New("drvload.exe is only available in the recovery environment (Windows PE)")
	}
	infs, err := findINFs(dir)
	if err != nil {
		return nil, err
	}
	if len(infs) == 0 {
		return nil, fmt.Errorf("no drivers (.inf files) in %s", dir)
	}
	res := &LoadDriversResult{}
	for _, inf := range infs {
		out, err := run(ctx, exe, inf)
		if err != nil {
			res.Failed = append(res.Failed, fmt.Sprintf("%s: %s", inf, lastLine(out, err)))
			continue
		}
		res.Loaded = append(res.Loaded, inf)
		log("loaded " + inf)
	}
	return res, nil
}

func findINFs(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.EqualFold(filepath.Ext(p), ".inf") {
			out = append(out, p)
		}
		return nil
	})
	return out, err
}

func lastLine(out string, err error) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if l := strings.TrimSpace(lines[len(lines)-1]); l != "" {
		return l
	}
	return err.Error()
}

// ProblemDevices lists devices without a working driver (pnputil
// /enum-devices /problem, available in Windows 10 2004 and later, also in
// Windows PE).
func ProblemDevices(ctx context.Context) ([]ProblemDevice, error) {
	out, err := run(ctx, "pnputil.exe", "/enum-devices", "/problem")
	if err != nil && !strings.Contains(out, ":") {
		return nil, fmt.Errorf("pnputil: %v: %s", err, strings.TrimSpace(out))
	}
	return parsePnputilDevices(out), nil
}

// AddDriversToWindows adds the drivers below dir to an installed Windows
// (offline, with DISM) without restoring anything, e.g. a storage driver
// so it starts on a new controller.
func AddDriversToWindows(ctx context.Context, wi WindowsInstall, dir string) (int, error) {
	if wi.volumeGUIDPath == "" {
		return 0, errors.New("unknown Windows installation")
	}
	win, unmount, err := letterFor(wi.volumeGUIDPath)
	if err != nil {
		return 0, fmt.Errorf("mount the Windows volume: %w", err)
	}
	defer unmount()
	return addDrivers(ctx, win, dir)
}
