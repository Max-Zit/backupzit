package pgsql

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"strconv"
)

// The agent runs as root, PostgreSQL's tools as postgres. In a folder the
// postgres account can write, root never follows a name postgres may have
// replaced with a link: files are created new (never an existing name, never
// through a link), read without following a link, and ownership is changed
// on the name itself. Otherwise a database superuser (COPY ... PROGRAM) or a
// compromised postgres account could make root write or chown any file on
// the server (H-N4).

// createNew creates p for writing; it fails when p exists, also as a link.
func createNew(p string, mode os.FileMode) (*os.File, error) {
	return os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL|oNoFollow, mode) // #nosec G304 -- a new file in the agent's working folder
}

// writeNew writes b to the new file p.
func writeNew(p string, b []byte, mode os.FileMode) error {
	f, err := createNew(p, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// putFile writes b to p (0600) in a folder only root can write to, in place
// of whatever is at that name: a link there (unpacked from an archive
// postgres wrote) is removed, never written through.
func putFile(p string, b []byte) error {
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return writeNew(p, b, 0o600)
}

// regularFile fails unless p is a regular file itself (not a link to one).
func regularFile(p string) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", p)
	}
	return nil
}

// readNoFollow reads p, refusing a link, at most limit bytes (0: no limit).
func readNoFollow(p string, limit int64) ([]byte, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|oNoFollow, 0) // #nosec G304 -- a file in the agent's working folder or WAL spool
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", p)
	}
	var r io.Reader = f
	if limit > 0 {
		r = io.LimitReader(f, limit)
	}
	return io.ReadAll(r)
}

// postgresIDs are the uid and gid of the postgres account.
func postgresIDs() (int, int, error) {
	u, err := user.Lookup("postgres")
	if err != nil {
		return 0, 0, err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, err
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return 0, 0, err
	}
	return uid, gid, nil
}

// givePostgres makes p (a new folder or file, not followed if it became a
// link) belong to postgres.
func givePostgres(p string) error {
	uid, gid, err := postgresIDs()
	if err != nil {
		return err
	}
	return os.Lchown(p, uid, gid)
}
