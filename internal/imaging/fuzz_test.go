package imaging

import (
	"encoding/binary"
	"testing"

	"github.com/max-zit/backupzit/internal/repo"
)

// FuzzParsePnputilDevices: the output of pnputil comes from a program the
// recovery environment runs; it must never panic the parser.
func FuzzParsePnputilDevices(f *testing.F) {
	f.Add("Instance ID: PCI\\VEN_8086\nDevice Description: Controller\nClass Name: SCSIAdapter\nStatus: Problem\nProblem Code: 28 (0x1C) [CM_PROB_FAILED_INSTALL]\n\n")
	f.Add("\n\n\n")
	f.Add("a: b")
	f.Fuzz(func(t *testing.T, out string) {
		for _, d := range parsePnputilDevices(out) {
			_ = d.InstanceID
		}
	})
}

// FuzzSetLayoutEntry: a drive layout read from the disk (DRIVE_LAYOUT_
// INFORMATION_EX) is parsed and changed; any bytes must give an error or a
// layout, never a panic or a write outside the buffer.
func FuzzSetLayoutEntry(f *testing.F) {
	seed := make([]byte, layoutHeader+2*layoutEntry)
	binary.LittleEndian.PutUint32(seed[0:], layoutStyleGPT)
	binary.LittleEndian.PutUint32(seed[4:], 2)
	binary.LittleEndian.PutUint32(seed[layoutHeader+24:], 1)
	f.Add(seed, 0, uint64(1<<20), uint64(100<<20))
	mbr := make([]byte, layoutHeader+4*layoutEntry)
	binary.LittleEndian.PutUint32(mbr[4:], 4)
	f.Add(mbr, 1, uint64(1<<20), uint64(1<<20))
	f.Add([]byte{1, 2, 3}, 0, uint64(0), uint64(0))
	f.Fuzz(func(t *testing.T, raw []byte, replace int, off, length uint64) {
		w := partitionWrite{Src: &repo.PartitionImage{Number: 1, GPTType: GPTTypeBasicData, Name: "x"}, Offset: off, Length: length}
		out, err := setLayoutEntry(raw, w, replace, "11111111-2222-3333-4444-555555555555", 512)
		if err == nil && len(out) < layoutHeader {
			t.Fatalf("layout of %d bytes", len(out))
		}
	})
}
