//go:build linux || darwin || freebsd

package vmlock

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func init() { pollEvery = 10 * time.Millisecond }

// L-N12 (0.56): a lock file that cannot be used (here a planted link, which
// O_NOFOLLOW refuses) is logged instead of silently giving up the lock
// across processes; the lock in this process still holds.
func TestUnusableLockFileIsLogged(t *testing.T) {
	oldDir, oldWarn := Dir, warn
	defer func() { Dir, warn = oldDir, oldWarn }()
	Dir = t.TempDir()
	var logged []string
	warn = func(msg string, args ...any) { logged = append(logged, msg+" "+fmt.Sprint(args...)) }
	if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere"), filepath.Join(Dir, "pve-7.lock")); err != nil {
		t.Fatal(err)
	}
	unlock, err := Lock(context.Background(), "pve-7")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if len(logged) != 1 || !strings.Contains(logged[0], "pve-7") {
		t.Fatalf("logged %q", logged)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := Lock(ctx, "pve-7"); err == nil {
		t.Fatal("the in-process lock did not hold without the lock file")
	}
}
