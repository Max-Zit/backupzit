package server

import (
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/repo"
)

func TestWeakens(t *testing.T) {
	daily := `{"kind":"daily","times":["21:00"],"days":[1,2,3,4,5]}`
	chain := `{"kind":"chain","after_job":3}`
	for _, c := range []struct {
		name     string
		kind     string
		old, cur protectSet
		want     string // held, comma separated
	}{
		{"same", JobFiles, protectSet{Schedule: daily, Paths: []string{"/a"}}, protectSet{Schedule: daily, Paths: []string{"/a"}}, ""},
		{"to chain", JobFiles, protectSet{Schedule: daily}, protectSet{Schedule: chain}, "schedule"},
		{"chain to daily", JobFiles, protectSet{Schedule: chain}, protectSet{Schedule: daily}, ""},
		{"to manual", JobFiles, protectSet{Schedule: chain}, protectSet{Schedule: ""}, "schedule"},
		{"folder added", JobFiles, protectSet{Paths: []string{"/a"}}, protectSet{Paths: []string{"/a", "/b"}}, ""},
		{"folder removed", JobFiles, protectSet{Paths: []string{"/a", "/b"}}, protectSet{Paths: []string{"/a"}}, "paths"},
		{"all VMs to a list", JobVM, protectSet{Paths: []string{"*"}}, protectSet{Paths: []string{"100", "101"}}, "paths"},
		{"list to all VMs", JobVM, protectSet{Paths: []string{"100"}}, protectSet{Paths: []string{"*"}}, ""},
		{"all VMs except one", JobVM, protectSet{Paths: []string{"*"}}, protectSet{Paths: []string{"*"}, Excludes: []string{"102"}}, "excludes"},
		{"all databases to one", JobSQL, protectSet{}, protectSet{Paths: []string{"shop"}}, "paths"},
		{"one database to all", JobSQL, protectSet{Paths: []string{"shop"}}, protectSet{}, ""},
		{"exclude removed", JobFiles, protectSet{Excludes: []string{"*.tmp"}}, protectSet{}, ""},
		{"shorter retention", JobFiles, protectSet{Retention: repo.RetentionPolicy{KeepLast: 5}}, protectSet{Retention: repo.RetentionPolicy{KeepLast: 3}}, "retention"},
	} {
		held, _ := weakens(c.kind, c.old, c.cur)
		if got := strings.Join(held, ","); got != c.want {
			t.Errorf("%s: held %q, want %q", c.name, got, c.want)
		}
	}
}
