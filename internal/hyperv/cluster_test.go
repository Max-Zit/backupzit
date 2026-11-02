package hyperv

import (
	"slices"
	"testing"
)

func TestPick(t *testing.T) {
	here := []hostVM{{ID: 1, Clustered: true}, {ID: 2}, {ID: 3, Clustered: true}}
	for _, c := range []struct {
		name         string
		ids, excl    []int
		cluster      bool
		sel, missing []int
	}{
		{"all on a host", nil, nil, false, []int{1, 2, 3}, nil},
		{"all but excluded", nil, []int{2}, false, []int{1, 3}, nil},
		{"all of a cluster: clustered VMs only", nil, nil, true, []int{1, 3}, nil},
		{"all of a cluster without excluded", nil, []int{3}, true, []int{1}, nil},
		{"selected on a host: missing reported", []int{1, 9}, nil, false, []int{1}, []int{9}},
		{"selected on a cluster node: others are elsewhere", []int{3, 9}, nil, true, []int{3}, nil},
		{"selected, none here", []int{9}, nil, true, nil, nil},
	} {
		sel, missing := pick(here, c.ids, c.excl, c.cluster)
		if !slices.Equal(sel, c.sel) || !slices.Equal(missing, c.missing) {
			t.Errorf("%s: got %v %v, want %v %v", c.name, sel, missing, c.sel, c.missing)
		}
	}
}
