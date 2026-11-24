//go:build darwin

package archiver

import (
	"io/fs"
	"syscall"

	"github.com/max-zit/backupzit/internal/repo"
)

// sfDataless is SF_DATALESS (sys/stat.h): the file's content is not on the
// disk but with a file provider (iCloud Drive with Optimize Mac Storage);
// reading it asks the provider to download it.
const sfDataless = 0x40000000

func entryInCloud(fi fs.FileInfo, n *repo.Node) bool {
	if n.Unix != nil {
		return n.Unix.Flags&sfDataless != 0
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Flags&sfDataless != 0
}

func pathInCloud(p string) bool {
	var st syscall.Stat_t
	return syscall.Lstat(p, &st) == nil && st.Flags&sfDataless != 0
}
