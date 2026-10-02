package vmfs

import (
	"fmt"
	"io"
)

// The file system readers parse data from backed up disks, which an
// attacker inside a VM controls. A malformed file system must produce an
// error, not crash the console or agent, so every call is guarded.

func guard(err *error) {
	if p := recover(); p != nil {
		*err = fmt.Errorf("damaged or unsupported file system data: %v", p)
	}
}

type safeFS struct{ fs FS }

func (s safeFS) List(dir string) (out []Entry, err error) {
	defer guard(&err)
	return s.fs.List(dir)
}

func (s safeFS) Stat(p string) (e Entry, err error) {
	defer guard(&err)
	return s.fs.Stat(p)
}

func (s safeFS) ReadFile(p string) (r io.Reader, n int64, err error) {
	defer guard(&err)
	r, n, err = s.fs.ReadFile(p)
	if err == nil {
		r = safeReader{r}
	}
	return r, n, err
}

type safeReader struct{ r io.Reader }

func (s safeReader) Read(b []byte) (n int, err error) {
	defer guard(&err)
	return s.r.Read(b)
}
