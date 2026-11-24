package backend

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// failing breaks one step of a backend, as storage that refuses writes or
// returns other data than it was given.
type failing struct {
	Backend
	save, remove error
	garble       bool
	listed       int
}

func (f *failing) Save(ctx context.Context, name string, data []byte) error {
	if f.save != nil {
		return f.save
	}
	return f.Backend.Save(ctx, name, data)
}

func (f *failing) Load(ctx context.Context, name string) ([]byte, error) {
	b, err := f.Backend.Load(ctx, name)
	if f.garble && err == nil {
		b = append(b, '!')
	}
	return b, err
}

func (f *failing) Remove(ctx context.Context, name string) error {
	if f.remove != nil {
		return f.remove
	}
	return f.Backend.Remove(ctx, name)
}

func (f *failing) List(ctx context.Context, dir string) ([]string, error) {
	f.listed++
	return f.Backend.List(ctx, dir)
}

// The connection test the console and the agents share writes, reads back
// and removes a file and leaves nothing behind; on immutable storage it
// only lists; a failed step is named.
func TestCheckConnection(t *testing.T) {
	ctx := context.Background()
	open := func() (*failing, *Local) {
		l, err := OpenLocal(filepath.Join(t.TempDir(), "store"))
		if err != nil {
			t.Fatal(err)
		}
		return &failing{Backend: l}, l
	}
	left := func(l *Local) []string {
		names, err := l.List(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		return names
	}

	be, l := open()
	res, err := CheckConnection(ctx, be, "sftp://backup@nas/srv", 0)
	if err != nil || res.Immutable || res.Summary() != "connected, wrote, read and removed a test file" {
		t.Fatalf("test: %+v %v", res, err)
	}
	if n := left(l); len(n) != 0 {
		t.Errorf("left behind: %v", n)
	}

	// Immutable storage: only a listing, nothing written.
	for _, c := range []struct {
		loc  string
		days int
	}{{"hardened://repo:8500/office", 0}, {"s3://s3.example/bucket", 30}} {
		be, l := open()
		be.save = errors.New("must not write")
		res, err := CheckConnection(ctx, be, c.loc, c.days)
		if err != nil || !res.Immutable || be.listed != 1 || !strings.Contains(res.Summary(), "nothing written") {
			t.Errorf("%s: %+v %v (listed %d)", c.loc, res, err, be.listed)
		}
		if n := left(l); len(n) != 0 {
			t.Errorf("%s: written: %v", c.loc, n)
		}
	}

	for _, c := range []struct {
		name string
		set  func(*failing)
		want string
	}{
		{"write", func(f *failing) { f.save = errors.New("permission denied") }, "write a test file: permission denied"},
		{"read back", func(f *failing) { f.garble = true }, "read: the file read back differs"},
		{"remove", func(f *failing) { f.remove = errors.New("no delete right") }, "remove the test file: no delete right"},
	} {
		be, l := open()
		c.set(be)
		if _, err := CheckConnection(ctx, be, "smb://nas/backups", 0); err == nil || err.Error() != c.want {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
		if c.name == "read back" {
			if n := left(l); len(n) != 0 {
				t.Errorf("a file that read back wrong stays: %v", n)
			}
		}
	}
}
