package vmfs

import (
	"bytes"
	"testing"
)

// Guest disks are not trusted: partition tables, LVM metadata and file
// systems of a VM are read when its files are browsed.
func FuzzParseLVMConfig(f *testing.F) {
	f.Add("vg0 {\nid = \"x\"\nlogical_volumes {\nroot {\nsegment_count = 1\n}\n}\n}\n")
	f.Add("a = [1, \"b\"]\n{")
	f.Fuzz(func(t *testing.T, s string) {
		_, _ = parseLVMConfig(s)
	})
}

func FuzzVolumes(f *testing.F) {
	mbr := make([]byte, 1024)
	mbr[510], mbr[511] = 0x55, 0xaa
	mbr[446+4] = 0x83
	mbr[446+8] = 1
	mbr[446+12] = 1
	f.Add(mbr)
	f.Add(append([]byte("LABELONE"), make([]byte, 600)...))
	f.Fuzz(func(t *testing.T, disk []byte) {
		vols, _ := Volumes(bytes.NewReader(disk), int64(len(disk)))
		for _, v := range vols {
			if !v.Browsable() {
				continue
			}
			if fs, err := Open(v); err == nil {
				_, _ = fs.List("/")
			}
		}
	})
}
