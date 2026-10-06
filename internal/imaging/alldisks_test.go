package imaging

import "testing"

func TestInAllDisks(t *testing.T) {
	part := []Partition{{Number: 1}}
	for _, c := range []struct {
		d    Disk
		want bool
	}{
		{Disk{Style: "gpt", Partitions: part}, true},
		{Disk{Style: "mbr", Partitions: part, System: true}, true},
		{Disk{Style: "gpt", Partitions: part, Bus: "usb"}, false},
		{Disk{Style: "gpt", Partitions: part, Bus: "virtual-file"}, false},
		{Disk{Style: "mbr", Partitions: part, Bus: "sd"}, false},
		{Disk{Style: StyleRaw}, false},
		{Disk{Style: "gpt"}, false},
	} {
		if got := InAllDisks(c.d); got != c.want {
			t.Errorf("%+v: %v", c.d, got)
		}
	}
	for _, c := range [][2]int{{0, 3}, {1, 2}, {4, 128}} {
		if img, num := SplitPartRef(PartRef(c[0], c[1])); img != c[0] || num != c[1] {
			t.Errorf("PartRef(%d, %d) round trip: %d %d", c[0], c[1], img, num)
		}
	}
	if PartRef(0, 3) != 3 {
		t.Error("the first disk must keep plain partition numbers")
	}
}
