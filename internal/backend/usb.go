package backend

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Removable disks (rotating USB disks for offline copies) are addressed by
// their volume label, not by drive letter or mount point, which change:
//
//	usb://BZBACKUP*/BackupZit/<repo>   any connected disk whose label matches
//	usb://BZBACKUP2/BackupZit/<repo>   exactly this disk
//
// Labels are matched case-insensitively; "*" and "?" are wildcards.

// Volume is a mounted file system with a label.
type Volume struct {
	Label string
	Path  string // drive root (E:\) or mount point (/media/backup)
}

// ErrNoDisk is returned when no matching disk is connected.
var ErrNoDisk = errors.New("no matching backup disk is connected")

// ResolveUSB finds the disk for a usb:// location and returns the local
// directory and the location with the actual label (for later restores).
func ResolveUSB(location string) (dir, resolved string, err error) {
	u, err := url.Parse(location)
	if err != nil || u.Scheme != "usb" || u.Host == "" {
		return "", "", fmt.Errorf("invalid removable disk location %q (use usb://LABEL/path)", location)
	}
	pattern := strings.ToUpper(u.Host)
	vols, err := listVolumesFn()
	if err != nil {
		return "", "", fmt.Errorf("list disks: %w", err)
	}
	var match []Volume
	var seen []string
	for _, v := range vols {
		if v.Label == "" {
			continue
		}
		seen = append(seen, v.Label)
		if ok, _ := path.Match(pattern, strings.ToUpper(v.Label)); ok {
			match = append(match, v)
		}
	}
	if len(match) == 0 {
		sort.Strings(seen)
		other := "no other labeled disks are connected"
		if len(seen) > 0 {
			other = "connected: " + strings.Join(seen, ", ")
		}
		return "", "", fmt.Errorf("%w: connect the disk labeled %s (%s)", ErrNoDisk, u.Host, other)
	}
	if len(match) > 1 {
		var labels []string
		for _, m := range match {
			labels = append(labels, m.Label)
		}
		return "", "", fmt.Errorf("several backup disks are connected (%s); connect only one", strings.Join(labels, ", "))
	}
	v := match[0]
	rel := strings.Trim(u.Path, "/")
	dir = filepath.Join(v.Path, filepath.FromSlash(rel))
	r := url.URL{Scheme: "usb", Host: v.Label, Path: "/" + rel}
	return dir, r.String(), nil
}

// USBLabel returns the label part of a usb:// location.
func USBLabel(location string) string {
	if u, err := url.Parse(location); err == nil && u.Scheme == "usb" {
		return u.Host
	}
	return ""
}

// listVolumesFn is replaced in tests.
var listVolumesFn = listVolumes
