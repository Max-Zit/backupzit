package runinfo

import (
	"encoding/json"
	"testing"

	"github.com/max-zit/backupzit/internal/repo"
)

// The run summary counts the VMware disks read through hot-add.
func TestGuestsCountsHotAdd(t *testing.T) {
	sn := &repo.Snapshot{
		Images: []repo.DiskImage{{Partitions: []repo.PartitionImage{{Method: "changed-blocks"}}}, {Partitions: []repo.PartitionImage{{Method: "used-blocks"}}}},
		Guests: []repo.Guest{{Platform: "vmware", Type: "vm", VMID: 1, Name: "web", Disks: []repo.GuestDisk{
			{Key: "scsi0:0", Image: 0, Size: 1 << 30, Via: "hot-add"},
			{Key: "scsi0:1", Image: 1, Size: 1 << 30},
		}}},
	}
	var got struct {
		Guests []GuestSummary `json:"guests"`
	}
	if err := json.Unmarshal(Guests(sn), &got); err != nil || len(got.Guests) != 1 {
		t.Fatalf("summary: %v %+v", err, got)
	}
	if g := got.Guests[0]; g.HotAdd != 1 || g.Changed != 1 || g.Disks != 2 {
		t.Errorf("summary %+v, want 1 hot-add, 1 changed of 2 disks", g)
	}
}
