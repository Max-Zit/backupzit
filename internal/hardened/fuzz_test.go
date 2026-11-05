package hardened

import (
	"path/filepath"
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
