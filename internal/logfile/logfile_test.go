package logfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotateAndTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	l.MaxSize, l.Keep = 100, 2
	for i := 0; i < 20; i++ {
		fmt.Fprintf(l, "line %02d xxxxxxxx\n", i) // 18 bytes
	}
	l.Close()
	for _, p := range []string{path, path + ".1", path + ".2"} {
		st, err := os.Stat(p)
		if err != nil || st.Size() > 100 {
			t.Fatalf("%s: %v %v", p, st, err)
		}
	}
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Error("more old files than kept")
	}
	b, err := Tail(path, 2, 60)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(b), "line 19 xxxxxxxx\n") || !strings.HasPrefix(string(b), "line ") || len(b) > 60 {
		t.Errorf("tail %q", b)
	}
	// Reopening appends.
	l, _ = Open(path)
	l.MaxSize = 1000
	fmt.Fprintln(l, "after restart")
	l.Close()
	if b, _ := Tail(path, 2, 1000); !strings.Contains(string(b), "line 19") || !strings.HasSuffix(string(b), "after restart\n") {
		t.Errorf("after reopen %q", b)
	}
}
