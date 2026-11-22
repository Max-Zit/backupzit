//go:build darwin

package restorer

import (
	"testing"

	"golang.org/x/sys/unix"
)

// Only the flags a restore may set come from the backup: SF_* as root
// only, never UF_COMPRESSED or the kernel's own flags, which keep the
// value the new file has.
func TestWantFlags(t *testing.T) {
	all := ^uint32(0)
	for _, c := range []struct {
		name              string
		stored, cur, want uint32
		root              bool
		only              uint32
	}{
		{"hidden and nodump", unix.UF_HIDDEN | unix.UF_NODUMP, 0, unix.UF_HIDDEN | unix.UF_NODUMP, false, all},
		{"compressed is never set", unix.UF_COMPRESSED | unix.UF_HIDDEN, 0, unix.UF_HIDDEN, true, all},
		{"compressed of the new file stays", 0, unix.UF_COMPRESSED, unix.UF_COMPRESSED, true, all},
		{"SIP and iCloud flags are not set", unix.SF_RESTRICTED | unix.SF_NOUNLINK | unix.SF_DATALESS | unix.UF_TRACKED | unix.UF_DATAVAULT, 0, 0, true, all},
		{"SF as root", unix.SF_ARCHIVED | unix.SF_IMMUTABLE, 0, unix.SF_ARCHIVED | unix.SF_IMMUTABLE, true, all},
		{"SF not as a user", unix.SF_ARCHIVED | unix.UF_OPAQUE, 0, unix.UF_OPAQUE, false, all},
		{"flags the backup has not are cleared", 0, unix.UF_HIDDEN, 0, false, all},
		{"locks wait for the rename", unix.UF_IMMUTABLE | unix.UF_HIDDEN, 0, unix.UF_HIDDEN, false, ^uint32(lockFlags)},
		{"then only the locks", unix.UF_IMMUTABLE | unix.UF_APPEND | unix.UF_HIDDEN, unix.UF_HIDDEN, unix.UF_IMMUTABLE | unix.UF_APPEND | unix.UF_HIDDEN, false, lockFlags},
	} {
		if got := wantFlags(c.stored, c.cur, c.root, c.only); got != c.want {
			t.Errorf("%s: wantFlags(%#x, %#x) = %#x, want %#x", c.name, c.stored, c.cur, got, c.want)
		}
	}
}
