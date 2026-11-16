package hyperv

import "testing"

func TestDiskExcluded(t *testing.T) {
	for _, c := range []struct {
		pats []string
		file string
		want bool
	}{
		{[]string{"*-data.vhdx"}, `D:\VMs\SQL01\sql01-data.vhdx`, true},
		{[]string{"*-data.vhdx"}, `D:\VMs\SQL01\sql01-os.vhdx`, false},
		{[]string{"DATA.VHDX"}, `d:\vms\sql01\data.vhdx`, true},
		{[]string{`D:\VMs\SQL01\data.vhdx`}, `D:\VMs\SQL01\data.vhdx`, true},
		{[]string{`D:\VMs\SQL01\data.vhdx`}, `D:\VMs\SQL02\data.vhdx`, false},
		{[]string{`D:\Data\*.vhdx`}, `D:\Data\big.vhdx`, true},
		{[]string{`D:\Data\*.vhdx`}, `D:\Data\sub\big.vhdx`, false},
		{[]string{"  ", "x.vhdx"}, `C:\x.vhdx`, true},
		{[]string{"[bad"}, `C:\[bad`, true}, // invalid pattern: literal
		{nil, `C:\x.vhdx`, false},
	} {
		if got := DiskExcluded(c.pats, c.file); got != c.want {
			t.Errorf("DiskExcluded(%q, %q) = %v, want %v", c.pats, c.file, got, c.want)
		}
	}
}
