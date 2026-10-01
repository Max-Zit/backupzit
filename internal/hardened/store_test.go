package hardened

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestStore(t *testing.T) {
	s, err := OpenStore(t.TempDir(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CheckImmutable(); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s.now = func() time.Time { return now }
	save := func(name, content string) error { return s.Save(name, strings.NewReader(content)) }
	read := func(name string, asOf int64) (string, error) {
		f, err := s.Open(name, asOf)
		if err != nil {
			return "", err
		}
		defer f.Close()
		b, err := io.ReadAll(f)
		return string(b), err
	}

	for _, bad := range []string{"", "/abs", "a/../b", "a//b", ".backupzit/deleted.log", "a/.tmp-1", "a b", "a\\b"} {
		if err := save(bad, "x"); !errors.Is(err, ErrInvalidName) {
			t.Errorf("name %q: %v", bad, err)
		}
	}
	if err := save("pc1/data/ab/abcd", "pack"); err != nil {
		t.Fatal(err)
	}
	if err := save("pc1/data/ab/abcd", "pack"); err != nil {
		t.Errorf("retried identical upload: %v", err)
	}
	if err := save("pc1/data/ab/abcd", "evil"); !errors.Is(err, ErrExists) {
		t.Errorf("overwrite: %v", err)
	}
	if got, _ := read("pc1/data/ab/abcd", 0); got != "pack" {
		t.Errorf("content %q", got)
	}
	fi, _ := os.Stat(s.path("pc1/data/ab/abcd"))
	if want := now.Add(8 * 24 * time.Hour); !fi.ModTime().Equal(want.Truncate(time.Second)) && fi.ModTime().Sub(want).Abs() > time.Second {
		t.Errorf("retain until %v, want %v", fi.ModTime(), want)
	}

	// Lock files are replaced and deleted freely.
	if err := save("pc1/locks/l1", "a"); err != nil {
		t.Fatal(err)
	}
	if err := save("pc1/locks/l1", "b"); err != nil {
		t.Errorf("lock refresh: %v", err)
	}
	if err := s.Remove("pc1/locks/l1"); err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(s.path("pc1/locks/l1")); !os.IsNotExist(err) {
		t.Error("lock file not removed")
	}

	// Deleting a retained file only hides it.
	save("pc1/snapshots/s1", "snap")
	deletedAt := now.Unix()
	if err := s.Remove("pc1/snapshots/s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := read("pc1/snapshots/s1", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("hidden file readable: %v", err)
	}
	if err := s.Remove("pc1/snapshots/s1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}
	if got, err := read("pc1/snapshots/s1", deletedAt-1); got != "snap" {
		t.Errorf("point-in-time read: %q %v", got, err)
	}
	if l, _ := s.List("pc1", 0); len(l) != 1 || l[0] != "pc1/data/ab/abcd" {
		t.Errorf("list: %v", l)
	}
	if l, _ := s.List("pc1", deletedAt-1); len(l) != 2 {
		t.Errorf("list as of: %v", l)
	}
	if n, _ := s.Sweep(); n != 0 {
		t.Error("sweep removed a retained file")
	}

	// The delete log survives a restart; undelete brings files back.
	s2, err := OpenStore(s.root, 7)
	if err != nil {
		t.Fatal(err)
	}
	s2.now = s.now
	if _, err := s2.Open("pc1/snapshots/s1", 0); !errors.Is(err, ErrNotFound) {
		t.Error("delete not persisted")
	}
	if n, err := s2.Undelete(time.Unix(deletedAt, 0)); n != 1 || err != nil {
		t.Fatalf("undelete: %d %v", n, err)
	}
	if got, _ := read("pc1/snapshots/s1", 0); got != "snap" {
		t.Error("undelete not seen by the running store")
	}

	// Keep extends files that would expire within the period.
	now = now.Add(3 * 24 * time.Hour)
	if n, err := s.Keep([]string{"pc1/data/ab/abcd", "pc1/snapshots/s1"}); n != 2 || err != nil {
		t.Errorf("keep: %d %v", n, err)
	}
	if n, _ := s.Keep([]string{"pc1/data/ab/abcd"}); n != 0 {
		t.Error("kept twice")
	}
	if _, err := s.Keep([]string{"pc1/missing"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("keep missing: %v", err)
	}

	// After the retention ends, deleted files are removed for real.
	s.Remove("pc1/snapshots/s1")
	now = now.Add(30 * 24 * time.Hour)
	if n, err := s.Sweep(); n != 1 || err != nil {
		t.Errorf("sweep: %d %v", n, err)
	}
	if _, err := os.Stat(s.path("pc1/snapshots/s1")); !os.IsNotExist(err) {
		t.Error("expired file still on disk")
	}
	// Expired files that were never deleted go away immediately.
	if err := s.Remove("pc1/data/ab/abcd"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.path("pc1/data/ab/abcd")); !os.IsNotExist(err) {
		t.Error("expired file not removed")
	}
	if st, _ := s.Stats(); st.Files != 0 || st.Hidden != 0 {
		t.Errorf("stats: %+v", st)
	}
}

func TestKeys(t *testing.T) {
	k := NewKeys(t.TempDir() + "/keys")
	tok, err := k.Add("console")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.Add("console"); err == nil {
		t.Error("duplicate key name")
	}
	tok2, _ := k.Add("office")
	if n, ok := k.Check(tok); !ok || n != "console" {
		t.Error("key rejected")
	}
	if _, ok := k.Check(tok + "x"); ok {
		t.Error("wrong key accepted")
	}
	if err := k.Remove("console"); err != nil {
		t.Fatal(err)
	}
	if _, ok := k.Check(tok); ok {
		t.Error("revoked key accepted")
	}
	if n, ok := k.Check(tok2); !ok || n != "office" {
		t.Error("other key lost")
	}
}
