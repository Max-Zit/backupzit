package server

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/max-zit/backupzit/internal/pve"
)

// TestSplitVMware: each VM goes to the proxy on its ESXi host, the rest to
// the connection's proxy, by the job's own choice ("*" with exclusions or
// a list, also by the former BIOS number).
func TestSplitVMware(t *testing.T) {
	inv := pve.Inventory{Platform: "vmware", Node: "vc", Version: "vCenter 8.0", Hosts: []string{"esx1", "esx2", "esx3"}, Guests: []pve.Guest{
		{VMID: 1, Name: "proxy-a", Node: "esx1", BIOSUUID: "aaaaaaaa-0000-0000-0000-000000000001"},
		{VMID: 2, Name: "proxy-b", Node: "esx2", BIOSUUID: "bbbbbbbb-0000-0000-0000-000000000002"},
		{VMID: 10, Name: "web", Node: "esx1"},
		{VMID: 11, Name: "db", Node: "esx2", FormerVMID: 911},
		{VMID: 12, Name: "mail", Node: "esx3"},
		{VMID: 13, Name: "lost", Node: "vc"}, // vCenter did not say where
	}}
	raw, _ := json.Marshal(inv)
	primary := int64(100)
	h := VMwareHost{ProxyAgentID: &primary, ExtraProxies: []int64{200, 300}, Inventory: raw}
	uuids := map[int64]string{
		100: "aaaaaaaa-0000-0000-0000-000000000001", // the connection's proxy on esx1
		// proxy 200 runs in proxy-b on esx2
		200: "bbbbbbbb-0000-0000-0000-000000000002",
		300: "", // an older agent: no machine UUID, no part
	}
	uuid := func(id int64) string { return uuids[id] }

	parts := splitVMware(h, Job{Paths: []string{"*"}, Excludes: []string{"12"}}, uuid)
	want := []vmwarePart{
		{agentID: 100, paths: []string{"*"}, excludes: []string{"11", "12", "2", "911"}, primary: true},
		{agentID: 200, paths: []string{"2", "11"}},
	}
	if !reflect.DeepEqual(parts, want) {
		t.Fatalf("all VMs:\n got %+v\nwant %+v", parts, want)
	}

	// A list, one VM named by its former BIOS number.
	parts = splitVMware(h, Job{Paths: []string{"10", "911", "12"}}, uuid)
	want = []vmwarePart{
		{agentID: 100, paths: []string{"10", "12"}, primary: true},
		{agentID: 200, paths: []string{"11"}},
	}
	if !reflect.DeepEqual(parts, want) {
		t.Fatalf("list:\n got %+v\nwant %+v", parts, want)
	}

	// Only VMs of the further proxy's host: the connection's proxy has no part.
	parts = splitVMware(h, Job{Paths: []string{"11"}}, uuid)
	want = []vmwarePart{{agentID: 200, paths: []string{"11"}}}
	if !reflect.DeepEqual(parts, want) {
		t.Fatalf("one host:\n got %+v\nwant %+v", parts, want)
	}

	// A VM unknown to the inventory stays with the connection's proxy (it
	// reports it missing, or it was added since).
	parts = splitVMware(h, Job{Paths: []string{"11", "99"}}, uuid)
	if len(parts) != 2 || !reflect.DeepEqual(parts[0].paths, []string{"99"}) || !parts[0].primary {
		t.Fatalf("unknown VM: %+v", parts)
	}

	// Two proxies on one host: the connection's own keeps it.
	uuids[200] = "aaaaaaaa-0000-0000-0000-000000000001"
	if parts := splitVMware(h, Job{Paths: []string{"*"}}, uuid); len(parts) != 1 || parts[0].agentID != 100 {
		t.Fatalf("shared host: %+v", parts)
	}

	// No further proxy or no inventory: not split.
	if splitVMware(VMwareHost{ProxyAgentID: &primary, Inventory: raw}, Job{Paths: []string{"*"}}, uuid) != nil {
		t.Error("split without further proxies")
	}
	if splitVMware(VMwareHost{ProxyAgentID: &primary, ExtraProxies: []int64{200}}, Job{Paths: []string{"*"}}, uuid) != nil {
		t.Error("split without inventory")
	}
}
