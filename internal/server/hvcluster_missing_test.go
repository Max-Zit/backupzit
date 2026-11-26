package server

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/pve"
)

func TestMissingClusterVMInventory(t *testing.T) {
	now := time.Now()
	guest := func(id int, clustered bool) pve.Guest { return pve.Guest{VMID: id, Clustered: clustered} }
	node := func(id int64, cluster string, guests ...pve.Guest) Agent {
		b, err := json.Marshal(pve.Inventory{Platform: "hyperv", ClusterName: cluster, Guests: append([]pve.Guest{}, guests...)})
		if err != nil {
			t.Fatal(err)
		}
		return Agent{ID: id, Hypervisor: b, HypervisorAt: &now, LastSeen: &now}
	}
	g := JobGroup{ID: 1, Name: "cluster", Enabled: true, Kind: JobVM, Scope: GroupHVCluster,
		Options: JobOptions{HVCluster: "HVC"}, Paths: []string{"102", "101", "102"}}
	a := node(1, "hvc", guest(101, true))
	b := node(2, "HVC", guest(102, true))
	removed := b
	removed.RemovedAt = &now
	recovery := b
	recovery.Recovery = true
	offline := b
	old := now.Add(-time.Hour)
	offline.LastSeen = &old
	stale := b
	stale.HypervisorAt = &old
	noTimestamp := b
	noTimestamp.HypervisorAt = nil
	incomplete := b
	incomplete.Hypervisor = json.RawMessage(`{"platform":"hyperv","cluster_name":"HVC","guests":null}`)
	future := b
	later := now.Add(time.Hour)
	future.HypervisorAt = &later
	excluded := g
	excluded.ExcludeIDs = []int64{2}
	disabled := g
	disabled.Enabled = false
	wildcard := g
	wildcard.Paths = []string{"*"}
	otherKind := g
	otherKind.Kind = JobFiles
	for _, tc := range []struct {
		name             string
		group            JobGroup
		agents           []Agent
		missing, unknown []int
	}{
		{"both nodes", g, []Agent{a, b}, nil, nil},
		{"same cluster migration", g, []Agent{node(1, "HVC", guest(101, true), guest(102, true)), node(2, "HVC")}, nil, nil},
		{"other cluster", g, []Agent{a, node(2, "OTHER", guest(102, true))}, []int{102}, nil},
		{"unclustered guest", g, []Agent{a, node(2, "HVC", guest(102, false))}, []int{102}, nil},
		{"deleted guest", g, []Agent{a, node(2, "HVC")}, []int{102}, nil},
		{"excluded node", excluded, []Agent{a, b}, []int{102}, nil},
		{"removed agent", g, []Agent{a, removed}, []int{102}, nil},
		{"recovery agent", g, []Agent{a, recovery}, []int{102}, nil},
		{"offline node with cached guest", g, []Agent{a, offline}, nil, []int{102}},
		{"online stale inventory", g, []Agent{a, stale}, nil, []int{102}},
		{"missing timestamp", g, []Agent{a, noTimestamp}, nil, []int{102}},
		{"incomplete guest list", g, []Agent{a, incomplete}, nil, []int{102}},
		{"future timestamp", g, []Agent{a, future}, nil, []int{102}},
		{"fresh presence despite stale node", g, []Agent{node(1, "HVC", guest(101, true), guest(102, true)), stale}, nil, nil},
		{"no matching nodes", g, nil, nil, []int{101, 102}},
		{"disabled", disabled, nil, nil, nil},
		{"wildcard", wildcard, nil, nil, nil},
		{"other kind", otherKind, nil, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs := observeClusterVMs(tc.group, clusterVMInventories(tc.agents, now))
			var missing, unknown []int
			for _, v := range obs {
				switch v.Status {
				case clusterVMMissing:
					missing = append(missing, v.ID)
				case clusterVMUnknown:
					unknown = append(unknown, v.ID)
				}
			}
			if !slices.Equal(missing, tc.missing) || !slices.Equal(unknown, tc.unknown) {
				t.Errorf("missing/unknown = %v/%v, want %v/%v", missing, unknown, tc.missing, tc.unknown)
			}
		})
	}
}

func TestMissingClusterVMWarnings(t *testing.T) {
	g := JobGroup{ID: 1, Name: "cluster", Options: JobOptions{HVCluster: "HVC"}}
	observed := []clusterVMObservation{
		{ID: 100, Status: clusterVMPresent},
		{ID: 101, Name: "db", Status: clusterVMMissing},
		{ID: 102, Status: clusterVMUnknown},
		{ID: 103, Status: clusterVMUnknown},
	}
	warnings := clusterVMWarnings(languages[0], g, observed)
	if len(warnings) != 2 || !strings.Contains(warnings[0], "VM 101 ") || !strings.Contains(warnings[0], "db") || !strings.Contains(warnings[1], "102, 103") {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if got := clusterVMWarnings(languages[0], g, observed[:1]); len(got) != 0 {
		t.Fatalf("present VM produced warnings: %v", got)
	}
}
