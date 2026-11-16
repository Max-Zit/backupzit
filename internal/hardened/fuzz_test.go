package hardened

import (
	"path/filepath"
	"strings"
	"testing"
)

// Object names come from the clients of the repository server: what
// ValidName accepts must stay below the storage directory.
func FuzzValidName(f *testing.F) {
	for _, s := range []string{"repo/data/ab/abcd", "../x", "a/./b", "a//b", ".hidden", `a\b`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		if ValidName(name) && !filepath.IsLocal(filepath.FromSlash(name)) {
			t.Fatalf("%q is accepted but leaves the storage directory", name)
		}
	})
}

// Lines of the key file: what parses must write back to the same key and
// limits, and a path-bound key never allows a name outside its path.
func FuzzParseKeyLine(f *testing.F) {
	h := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for _, s := range []string{"pc1 " + h, "pc1 " + h + " path=office/pc1 quota=5 daily=7 rate=3", "pc1 " + h + " path=../x", "# x " + h, "pc1 " + h + " qouta=1"} {
		f.Add(s, "office/pc1/data/ab")
	}
	f.Fuzz(func(t *testing.T, line, name string) {
		e, ok := parseKeyLine(line)
		if !ok {
			return
		}
		e2, ok2 := parseKeyLine(e.line())
		if !ok2 || e2.name != e.name || e2.lim != e.lim || string(e2.hash) != string(e.hash) {
			t.Fatalf("%q does not round-trip: %+v -> %q", line, e, e.line())
		}
		if p := e.lim.Path; p != "" && e.lim.Allows(name) && name != p && !strings.HasPrefix(name, p+"/") {
			t.Fatalf("key path %q allows %q", p, name)
		}
	})
}
