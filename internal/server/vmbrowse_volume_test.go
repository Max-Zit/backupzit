package server

import (
	"testing"

	"github.com/max-zit/backupzit/internal/vmfs"
)

// B10 (round 4): switching the disk sent the partition of the previous
// disk along ("no volume p3"). The browse page falls back to the first
// browsable partition when the one asked for is not on this disk.
func TestHasVolume(t *testing.T) {
	vols := []vmfs.Volume{{ID: "p1"}, {ID: "p2"}}
	if !hasVolume(vols, "p2") {
		t.Error("p2 is on the disk")
	}
	if hasVolume(vols, "p3") || hasVolume(nil, "p1") {
		t.Error("p3 is not on the disk")
	}
}
