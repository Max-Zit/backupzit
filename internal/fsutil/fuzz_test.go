package fsutil

import "testing"

// Snapshot paths come from restore forms and the command line.
func FuzzParseSnapshotPath(f *testing.F) {
	f.Add("/C:/Users/ana/Documents")
	f.Add("/srv/data/../x")
	f.Add("//")
	f.Fuzz(func(t *testing.T, p string) {
		comps, err := ParseSnapshotPath(p)
		if err != nil {
			return
		}
		for _, c := range comps {
			if c == "" || c == "." {
				t.Fatalf("%q gave the component %q", p, c)
			}
		}
		_ = OriginalPath(comps)
	})
}
