package sysbackup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

type lsblkDev struct {
	Name        string     `json:"name"`
	Path        string     `json:"path"`
	Type        string     `json:"type"`
	Size        uint64     `json:"size"`
	FSType      string     `json:"fstype"`
	FSVer       string     `json:"fsver"`
	UUID        string     `json:"uuid"`
	PartUUID    string     `json:"partuuid"`
	Label       string     `json:"label"`
	PartLabel   string     `json:"partlabel"`
	PartType    string     `json:"parttype"`
	PartFlags   string     `json:"partflags"`
	PTType      string     `json:"pttype"`
	PTUUID      string     `json:"ptuuid"`
	MountPoints []*string  `json:"mountpoints"`
	LogSec      uint32     `json:"log-sec"`
	Model       string     `json:"model"`
	FSUsed      uint64     `json:"fsused"`
	Children    []lsblkDev `json:"children"`
}

// lsblkNum is a number in lsblk's JSON output. util-linux 2.37 (RHEL,
// Rocky and Alma 9, Ubuntu 22.04) writes some of them as strings
// ("fsused": "4789460992"), newer versions as numbers; a column without a
// value is null. Anything else is an error: a size read wrongly would
// size the file systems of a restore wrongly.
type lsblkNum uint64

func (n *lsblkNum) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if bytes.Equal(b, []byte("null")) {
		*n = 0
		return nil
	}
	s := string(b)
	if len(b) >= 2 && b[0] == '"' {
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if s == "" {
			*n = 0
			return nil
		}
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return fmt.Errorf("not a whole number: %.40q", s)
	}
	*n = lsblkNum(v)
	return nil
}

func (d *lsblkDev) UnmarshalJSON(b []byte) error {
	type plain lsblkDev // without this method
	var aux struct {
		plain
		// Shallower fields win over the embedded ones of the same name.
		Size   lsblkNum `json:"size"`
		LogSec lsblkNum `json:"log-sec"`
		FSUsed lsblkNum `json:"fsused"`
	}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	if aux.LogSec > 1<<20 {
		return fmt.Errorf("lsblk: implausible sector size %d of %s", aux.LogSec, aux.Name)
	}
	*d = lsblkDev(aux.plain)
	d.Size, d.LogSec, d.FSUsed = uint64(aux.Size), uint32(aux.LogSec), uint64(aux.FSUsed) // #nosec G115 -- LogSec checked above
	return nil
}

// parseLsblk decodes the output of lsblk -J.
func parseLsblk(out []byte) ([]lsblkDev, error) {
	var tree struct {
		BlockDevices []lsblkDev `json:"blockdevices"`
	}
	if err := json.Unmarshal(out, &tree); err != nil {
		return nil, fmt.Errorf("parse lsblk: %w", err)
	}
	return tree.BlockDevices, nil
}
func (d lsblkDev) mounts() []string {
	var out []string
	for _, m := range d.MountPoints {
		if m != nil && *m != "" {
			out = append(out, *m)
		}
	}
	return out
}
