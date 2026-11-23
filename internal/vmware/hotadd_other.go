//go:build !linux

package vmware

import (
	"context"
	"errors"
)

var errHotAddLinux = errors.New("hot-add needs the Linux agent in a VM; this proxy agent copies the disks on the datastore")

var ownVMUUIDs = func() ([]string, error) { return nil, errHotAddLinux }

var blockDevices = func() (map[string]uint64, error) { return nil, errHotAddLinux }

var waitForDisk = func(context.Context, map[string]uint64, uint64) (string, error) {
	return "", errHotAddLinux
}

var releaseDisk = func(string, func(string, ...any)) {}

var lockHotAdd = func() (func(), error) { return nil, errHotAddLinux }
