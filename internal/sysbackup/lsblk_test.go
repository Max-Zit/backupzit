package sysbackup

import (
	"strings"
	"testing"
)

// util-linux 2.37 (Rocky/Alma/RHEL 9, Ubuntu 22.04) writes fsused as a
// string, newer versions as a number (issue #35).
const lsblk237 = `{
   "blockdevices": [
      {"name":"sda", "path":"/dev/sda", "type":"disk", "size":107374182400, "fstype":null, "fsver":null, "uuid":null, "partuuid":null, "label":null, "partlabel":null, "parttype":null, "partflags":null, "pttype":"gpt", "ptuuid":"0f6c", "mountpoints":[null], "log-sec":512, "model":"Virtual Disk", "fsused":null,
         "children": [
            {"name":"sda1", "path":"/dev/sda1", "type":"part", "size":629145600, "fstype":"vfat", "fsver":"FAT32", "uuid":"7B77-95E7", "partuuid":"aa", "label":null, "partlabel":"EFI System Partition", "parttype":"c12a7328-f81f-11d2-ba4b-00a0c93ec93b", "partflags":null, "pttype":"gpt", "ptuuid":"0f6c", "mountpoints":["/boot/efi"], "log-sec":512, "model":null, "fsused":"7364608"},
            {"name":"sda3", "path":"/dev/sda3", "type":"part", "size":"105670066176", "fstype":"xfs", "fsver":null, "uuid":"9a1c", "partuuid":"cc", "label":null, "partlabel":null, "parttype":"0fc63daf-8483-4772-8e79-3d69d8477de4", "partflags":null, "pttype":"gpt", "ptuuid":"0f6c", "mountpoints":["/"], "log-sec":"512", "model":null, "fsused":"4789460992"}
         ]
      }
   ]
}`

const lsblk241 = `{"blockdevices":[{"name":"vda","path":"/dev/vda","type":"disk","size":34359738368,"log-sec":512,"fsused":null,"mountpoints":[null],
  "children":[{"name":"vda1","path":"/dev/vda1","type":"part","size":34358689792,"fstype":"ext4","log-sec":512,"fsused":3263021056,"mountpoints":["/"]}]}]}`

func TestParseLsblk(t *testing.T) {
	for name, tc := range map[string]struct {
		in         string
		size, used uint64
	}{
		"util-linux 2.37 strings": {lsblk237, 105670066176, 4789460992},
		"util-linux 2.41 numbers": {lsblk241, 34358689792, 3263021056},
	} {
		t.Run(name, func(t *testing.T) {
			devs, err := parseLsblk([]byte(tc.in))
			if err != nil {
				t.Fatal(err)
			}
			if len(devs) != 1 || len(devs[0].Children) == 0 {
				t.Fatalf("tree %+v", devs)
			}
			last := devs[0].Children[len(devs[0].Children)-1]
			if last.Size != tc.size || last.FSUsed != tc.used || last.LogSec != 512 {
				t.Errorf("size %d used %d log-sec %d", last.Size, last.FSUsed, last.LogSec)
			}
			if devs[0].FSUsed != 0 || devs[0].Type != "disk" || len(last.mounts()) != 1 {
				t.Errorf("disk %+v", devs[0])
			}
		})
	}
}

// A value that is not a whole number is an error, never a silent 0: the
// sizes of a restore depend on it.
func TestParseLsblkRejects(t *testing.T) {
	for _, v := range []string{`"4.5G"`, `"-1"`, `true`, `"12 34"`, `1.5`, `"99999999999999999999999"`} {
		in := `{"blockdevices":[{"name":"sda","size":` + v + `}]}`
		if _, err := parseLsblk([]byte(in)); err == nil {
			t.Errorf("size %s accepted", v)
		}
	}
	if _, err := parseLsblk([]byte(`{"blockdevices":[{"name":"sda","size":"","fsused":null,"log-sec":"4096"}]}`)); err != nil {
		t.Errorf("empty and null values: %v", err)
	}
	if _, err := parseLsblk([]byte(`{"blockdevices":[{"name":"sda","log-sec":4294967296}]}`)); err == nil || !strings.Contains(err.Error(), "sector size") {
		t.Errorf("huge sector size: %v", err)
	}
}

func FuzzLsblkNum(f *testing.F) {
	for _, s := range []string{`1`, `"1"`, `null`, `""`, `"x"`, `-1`, `"18446744073709551615"`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		var n lsblkNum
		_ = n.UnmarshalJSON([]byte(s)) // must not panic
	})
}
