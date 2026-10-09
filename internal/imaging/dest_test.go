package imaging

import (
	"path/filepath"
	"testing"

	"github.com/max-zit/backupzit/internal/repo"
)

// Files restored from an image into a folder get the same layout as files
// restored from a file backup: the drive letter is the first folder.
func TestDestFunc(t *testing.T) {
	target := filepath.Join("restore", "here")
	for _, c := range []struct {
		mounts []string
		want   string
	}{
		{[]string{`C:\`}, filepath.Join(target, "C", "Users", "ana", "a.txt")},
		{[]string{`d:\`}, filepath.Join(target, "D", "Users", "ana", "a.txt")},
		{nil, filepath.Join(target, "Users", "ana", "a.txt")},
		{[]string{`\?\Volume{1234}\`}, filepath.Join(target, "Users", "ana", "a.txt")},
	} {
		f, err := DestFunc(&repo.PartitionImage{Number: 2, MountPoints: c.mounts}, target)
		if err != nil {
			t.Fatal(err)
		}
		if got := f(`\Users\ana\a.txt`); got != c.want {
			t.Errorf("%v: %s, want %s", c.mounts, got, c.want)
		}
	}
	// Original location: the partition's drive, no extra folder.
	f, err := DestFunc(&repo.PartitionImage{Number: 2, MountPoints: []string{`E:\`}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := f(`\Data\x.txt`), filepath.Join(`E:\`, "Data", "x.txt"); got != want {
		t.Errorf("original: %s, want %s", got, want)
	}
	if _, err := DestFunc(&repo.PartitionImage{Number: 3}, ""); err == nil {
		t.Error("original location of a partition without a drive letter accepted")
	}
}
