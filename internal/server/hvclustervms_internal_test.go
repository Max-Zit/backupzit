package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/pve"
)

// #72: a selected VM counts as missing only when every node of the cluster
// job reported its VMs recently; a silent node makes it "cannot check".
func TestCheckClusterVMs(t *testing.T) {
	now := time.Now()
	node := func(id int64, seen, inv time.Time, guests ...pve.Guest) Agent {
		b, _ := json.Marshal(pve.Inventory{Platform: "hyperv", Node: "hv", ClusterName: "HVC", Guests: guests})
		return Agent{ID: id, Hostname: "hv", LastSeen: &seen, HypervisorAt: &inv, Hypervisor: b}
	}
	g := JobGroup{ID: 1, Name: "cluster", Enabled: true, Kind: JobVM, Scope: GroupHVCluster, Paths: []string{"101", "102", "103"},
		Options: JobOptions{HVCluster: "HVC"}}
	web := pve.Guest{VMID: 101, Name: "web", Clustered: true}
	db := pve.Guest{VMID: 102, Name: "db  server", Clustered: false} // still on a node, no longer a role
	fresh := []Agent{node(1, now, now, web), node(2, now, now.Add(-5*time.Minute), db)}
	c := checkClusterVMs(g, fresh, now)
	if strings.Join(c.Missing, ";") != "102 (db server);103" || len(c.Unknown) != 0 {
		t.Errorf("fresh nodes: %+v", c)
	}
	stale := []Agent{node(1, now, now, web), node(2, now, now.Add(-time.Hour), db)}
	if c := checkClusterVMs(g, stale, now); len(c.Missing) != 0 || strings.Join(c.Unknown, ",") != "102,103" {
		t.Errorf("stale node: %+v", c)
	}
	offline := []Agent{node(1, now, now, web), node(2, now.Add(-10*time.Minute), now, db)}
	if c := checkClusterVMs(g, offline, now); len(c.Missing) != 0 || len(c.Unknown) != 2 {
		t.Errorf("offline node: %+v", c)
	}
	all := g
	all.Paths = []string{"*"}
	if c := checkClusterVMs(all, fresh, now); len(c.Missing)+len(c.Unknown) != 0 {
		t.Errorf("all VMs: %+v", c)
	}
	excluded := g
	excluded.ExcludeIDs = []int64{2}
	if c := checkClusterVMs(excluded, fresh, now); strings.Join(c.Missing, ";") != "102;103" {
		// the excluded node does not count; its VM names are unknown
		t.Errorf("excluded node: %+v", c)
	}
	texts := clusterVMTexts(languageByCode("en"), g, clusterVMCheck{Missing: []string{"103"}, Unknown: []string{"104"}})
	if len(texts) != 2 || !strings.Contains(texts[0], "VM 103 is no longer a clustered VM of HVC") || !strings.Contains(texts[1], "cannot check VMs 104 of HVC") {
		t.Errorf("texts %q", texts)
	}
}
