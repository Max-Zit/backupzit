// Package vss creates Volume Shadow Copy snapshots so backups read a
// consistent, point-in-time view of the volume, including files that other
// programs keep open or locked.
package vss

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// Set is a group of snapshots, one per volume, that live for one backup.
type Set struct {
	snaps map[string]string // lower-case volume ("c:") -> snapshot device path
	close []func() error
}

// Map translates a path on a snapshotted volume into the same path inside
// the snapshot. Paths on other volumes are returned unchanged.
func (s *Set) Map(p string) string {
	if s == nil {
		return p
	}
	vol := filepath.VolumeName(p)
	dev, ok := s.snaps[strings.ToLower(vol)]
	if !ok {
		return p
	}
	return dev + p[len(vol):]
}

// Volumes returns the snapshotted volumes.
func (s *Set) Volumes() []string {
	if s == nil {
		return nil
	}
	var out []string
	for v := range s.snaps {
		out = append(out, strings.ToUpper(v))
	}
	return out
}

// Close deletes all snapshots of the set.
func (s *Set) Close() error {
	if s == nil {
		return nil
	}
	var first error
	for _, c := range s.close {
		if err := c(); err != nil && first == nil {
			first = err
		}
	}
	s.close = nil
	return first
}

// Create snapshots every local volume that contains one of paths. Volumes
// that cannot be snapshotted (network shares, FAT, missing privileges) are
// reported through onErr and backed up live.
func Create(paths []string, timeout time.Duration, onErr ErrorHandler) (*Set, error) {
	s := &Set{snaps: map[string]string{}}
	seen := map[string]bool{}
	for _, p := range paths {
		vol := strings.ToLower(filepath.VolumeName(p))
		if vol == "" || seen[vol] {
			continue
		}
		seen[vol] = true
		if len(vol) != 2 || vol[1] != ':' {
			onErr(p, fmt.Errorf("VSS is not available for %s (network or non-drive path); reading it without a snapshot", vol))
			continue
		}
		dev, closeFn, err := snapshotVolume(vol+`\`, timeout, onErr)
		if err != nil {
			onErr(vol, fmt.Errorf("VSS snapshot of %s failed, reading files without a snapshot: %w", strings.ToUpper(vol), err))
			continue
		}
		s.snaps[vol] = dev
		s.close = append(s.close, closeFn)
	}
	return s, nil
}
