package backend

import (
	"golang.org/x/sys/windows"
)

func listVolumes() ([]Volume, error) {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil, err
	}
	var out []Volume
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		p, _ := windows.UTF16PtrFromString(root)
		switch windows.GetDriveType(p) {
		case windows.DRIVE_REMOVABLE, windows.DRIVE_FIXED:
		default:
			continue // network shares, CD-ROMs
		}
		label := make([]uint16, windows.MAX_PATH+1)
		if err := windows.GetVolumeInformation(p, &label[0], uint32(len(label)), nil, nil, nil, nil, 0); err != nil {
			continue // e.g. empty card reader
		}
		out = append(out, Volume{Label: windows.UTF16ToString(label), Path: root})
	}
	return out, nil
}
