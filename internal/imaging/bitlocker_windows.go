//go:build windows

package imaging

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// LockedBitLockerVolumes lists the volumes whose first sector still shows
// the BitLocker header: an unlocked volume reads as its file system.
func LockedBitLockerVolumes() ([]LockedVolume, error) {
	disks, err := ListDisks()
	if err != nil {
		return nil, err
	}
	var out []LockedVolume
	for _, d := range disks {
		for _, p := range d.Partitions {
			if p.VolumeGUIDPath == "" {
				continue
			}
			h, err := openDevice(strings.TrimSuffix(p.VolumeGUIDPath, `\`), false)
			if err != nil {
				continue
			}
			f := os.NewFile(uintptr(h), p.VolumeGUIDPath)
			// Raw volume reads must be whole sectors.
			sec := make([]byte, 4096)
			_, rerr := f.ReadAt(sec, 0)
			_ = f.Close()
			if rerr == nil && IsBitLockerRaw(bytes.NewReader(sec)) {
				out = append(out, LockedVolume{Disk: d.Number, Partition: p.Number, Length: p.Length, guidPath: p.VolumeGUIDPath})
			}
		}
	}
	return out, nil
}

// UnlockBitLocker unlocks a locked volume with its 48-digit recovery key
// (manage-bde, from the WinPE-SecureStartup component). The volume keeps
// a drive letter so it can be used afterwards; it is returned.
func UnlockBitLocker(ctx context.Context, v LockedVolume, recoveryKey string) (string, error) {
	key := strings.TrimSpace(recoveryKey)
	if !ValidRecoveryKey(key) {
		return "", errors.New("a recovery key has 48 digits in 8 groups: 123456-123456-...")
	}
	exe, err := exec.LookPath("manage-bde.exe")
	if err != nil {
		return "", errors.New("manage-bde.exe is not available (Windows PE needs the WinPE-SecureStartup component)")
	}
	letter, _, err := letterFor(v.guidPath)
	if err != nil {
		return "", err
	}
	out, err := run(ctx, exe, "-unlock", letter, "-RecoveryPassword", key)
	if err != nil {
		return letter, fmt.Errorf("manage-bde: %v: %s", err, lastLine(out, err))
	}
	return letter, nil
}
