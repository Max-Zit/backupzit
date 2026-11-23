//go:build !windows

package backend

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A named pipe in the repository is not listed, and reading it fails at
// once instead of waiting for a writer.
func TestLocalNamedPipeIsNoObject(t *testing.T) {
	ctx := context.Background()
	l, root, _ := localRepo(t)
	if err := os.MkdirAll(filepath.Join(root, "index"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "index", "pipe"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if got, err := l.List(ctx, "index"); err != nil || len(got) != 0 {
		t.Fatalf("list: %v %v", got, err)
	}
	done := make(chan error, 1)
	go func() { _, err := l.Load(ctx, "index/pipe"); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a named pipe was read as an object")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading a named pipe hangs")
	}
	if _, err := l.Size(ctx, "index/pipe"); err == nil {
		t.Fatal("size of a named pipe")
	}
}
