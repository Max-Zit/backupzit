package archiver

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestExcluded(t *testing.T) {
	root := filepath.FromSlash("/srv/data")
	if runtime.GOOS == "windows" {
		root = `C:\Data`
	}
	p := func(rel string) string { return filepath.Join(root, filepath.FromSlash(rel)) }
	a := &Archiver{opts: Options{Excludes: []string{
		"*.tmp",                                // base name
		"node_modules",                         // base name, any depth
		filepath.ToSlash(p("cache")),           // full path in slash style
		p("logs") + string(filepath.Separator), // trailing separator
		filepath.ToSlash(p("vm")) + "/*.iso",   // pattern with a path
	}}}
	for _, c := range []struct {
		rel  string
		want bool
	}{
		{"a.tmp", true},
		{"x/y/node_modules", true},
		{"cache", true},
		{"logs", true},
		{"vm/disk.iso", true},
		{"vm/notes.txt", false},
		{"other/cache", false},
		{"docs/a.txt", false},
	} {
		if got := a.excluded(p(c.rel), filepath.Base(p(c.rel))); got != c.want {
			t.Errorf("%s: excluded = %v, want %v", c.rel, got, c.want)
		}
	}
	if runtime.GOOS == "windows" && !a.excluded(`c:\data\CACHE`, "CACHE") {
		t.Error("full-path exclusions should ignore case on Windows")
	}
}
