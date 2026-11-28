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

// #61: a disk that was never initialized (Windows reports it as an empty
// MBR) has no boot signature in its first sector; an MBR, a protective MBR
// and a 4 KiB sector with the signature at 510 have one.
func TestNoPartitionTable(t *testing.T) {
	signed := func(n int) []byte {
		b := make([]byte, n)
		b[510], b[511] = 0x55, 0xAA
		return b
	}
	for what, c := range map[string]struct {
		sec  []byte
		want bool
	}{
		"all zeros (new disk)":   {make([]byte, 512), true},
		"leftover data, no 55AA": {append(make([]byte, 510), 0xAA, 0x55), true},
		"MBR":                    {signed(512), false},
		"4 KiB sector with MBR":  {signed(4096), false},
		"4 KiB sector, zeros":    {make([]byte, 4096), true},
		"too short to tell":      {make([]byte, 100), false},
		"nothing read":           {nil, false},
	} {
		if got := noPartitionTable(c.sec); got != c.want {
			t.Errorf("%s: %v, want %v", what, got, c.want)
		}
	}
}
