//go:build !windows

package archiver

import (
	"fmt"
	"io/fs"
	"os/user"
	"strconv"
	"sync"
	"syscall"

	"github.com/backupzit/backupzit/internal/repo"
	"golang.org/x/sys/unix"
)

var (
	nameMu     sync.Mutex
	userNames  = map[uint32]string{}
	groupNames = map[uint32]string{}
)

func userName(uid uint32) string {
	nameMu.Lock()
	defer nameMu.Unlock()
	if n, ok := userNames[uid]; ok {
		return n
	}
	n := ""
	if u, err := user.LookupId(strconv.FormatUint(uint64(uid), 10)); err == nil {
		n = u.Username
	}
	userNames[uid] = n
	return n
}

func groupName(gid uint32) string {
	nameMu.Lock()
	defer nameMu.Unlock()
	if n, ok := groupNames[gid]; ok {
		return n
	}
	n := ""
	if g, err := user.LookupGroupId(strconv.FormatUint(uint64(gid), 10)); err == nil {
		n = g.Name
	}
	groupNames[gid] = n
	return n
}

// unixMeta reads owner, device number, hard link identity and extended
// attributes of p.
func unixMeta(p string, fi fs.FileInfo) *repo.UnixMeta {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	m := &repo.UnixMeta{UID: st.Uid, GID: st.Gid, User: userName(st.Uid), Group: groupName(st.Gid)}
	if fi.Mode()&fs.ModeDevice != 0 {
		m.Rdev = uint64(st.Rdev)
		m.Char = fi.Mode()&fs.ModeCharDevice != 0
	}
	if fi.Mode().IsRegular() && st.Nlink > 1 {
		m.LinkKey = fmt.Sprintf("%d:%d", st.Dev, st.Ino)
	}
	m.Xattrs = readXattrs(p)
	return m
}

func readXattrs(p string) map[string][]byte {
	size, err := unix.Llistxattr(p, nil)
	if err != nil || size <= 0 {
		return nil
	}
	buf := make([]byte, size)
	size, err = unix.Llistxattr(p, buf)
	if err != nil {
		return nil
	}
	out := map[string][]byte{}
	start := 0
	for i := 0; i < size; i++ {
		if buf[i] != 0 {
			continue
		}
		name := string(buf[start:i])
		start = i + 1
		if name == "" {
			continue
		}
		vs, err := unix.Lgetxattr(p, name, nil)
		if err != nil || vs < 0 {
			continue
		}
		val := make([]byte, vs)
		if vs > 0 {
			if vs, err = unix.Lgetxattr(p, name, val); err != nil {
				continue
			}
		}
		out[name] = val[:vs]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// deviceOf returns the file system device of fi.
func deviceOf(fi fs.FileInfo) (uint64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Dev), true
}
