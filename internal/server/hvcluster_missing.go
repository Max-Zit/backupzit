package server

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/pve"
)

const (
	// Agents normally report inventory every 15 minutes.
	clusterInventoryMaxAge = 20 * time.Minute
	clusterAgentMaxAge     = 3 * time.Minute
)

type clusterVMStatus int

const (
	clusterVMUnknown clusterVMStatus = iota
	clusterVMPresent
	clusterVMMissing
)

type clusterVMObservation struct {
	ID     int
	Name   string
	Status clusterVMStatus
}

type clusterVMInventory struct {
	agent Agent
	inv   *pve.Inventory
	fresh bool
}

func clusterVMRecent(at *time.Time, now time.Time, maxAge time.Duration) bool {
	return at != nil && !at.After(now.Add(time.Minute)) && now.Sub(*at) < maxAge
}

func clusterVMInventories(agents []Agent, now time.Time) []clusterVMInventory {
	var out []clusterVMInventory
	for _, a := range agents {
		if inv := a.PVE(); inv != nil && inv.Platform == "hyperv" && inv.ClusterName != "" {
			out = append(out, clusterVMInventory{a, inv, inv.Guests != nil &&
				clusterVMRecent(a.LastSeen, now, clusterAgentMaxAge) && clusterVMRecent(a.HypervisorAt, now, clusterInventoryMaxAge)})
		}
	}
	return out
}

func monitorsClusterVMs(g JobGroup) bool {
	return g.Enabled && g.Kind == JobVM && g.Scope == GroupHVCluster && g.Options.HVCluster != "" && len(g.Paths) > 0 && !slices.Contains(g.Paths, "*")
}

func observeClusterVMs(g JobGroup, inventories []clusterVMInventory) []clusterVMObservation {
	if !monitorsClusterVMs(g) {
		return nil
	}
	present := map[int]bool{}
	names := map[int]string{}
	complete, nodes := true, 0
	for _, node := range inventories {
		if !g.matchesHVCluster(node.agent, node.inv.ClusterName) {
			continue
		}
		nodes++
		if !node.fresh {
			complete = false
		}
		for _, vm := range node.inv.Guests {
			if node.fresh && vm.Name != "" {
				name := []rune(strings.Join(strings.Fields(vm.Name), " "))
				names[vm.VMID] = string(name[:min(len(name), 100)])
			}
			if node.fresh && vm.Clustered {
				present[vm.VMID] = true
			}
		}
	}
	var ids []int
	for _, p := range g.Paths {
		if id, err := strconv.Atoi(p); err == nil && id >= 100 {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	var out []clusterVMObservation
	for _, id := range slices.Compact(ids) {
		status := clusterVMUnknown
		if present[id] {
			status = clusterVMPresent
		} else if complete && nodes > 0 {
			status = clusterVMMissing
		}
		out = append(out, clusterVMObservation{id, names[id], status})
	}
	return out
}

func missingClusterVMText(l *language, g JobGroup, id int, name string) string {
	text := l.T("Selected VM %d is not reported as clustered by any participating node of Hyper-V cluster %s.", id, g.Options.HVCluster)
	if name != "" {
		text += " — " + name
	}
	return text
}

func clusterVMWarnings(l *language, g JobGroup, observed []clusterVMObservation) []string {
	var out, unknown []string
	for _, vm := range observed {
		switch vm.Status {
		case clusterVMMissing:
			out = append(out, missingClusterVMText(l, g, vm.ID, vm.Name))
		case clusterVMUnknown:
			unknown = append(unknown, strconv.Itoa(vm.ID))
		}
	}
	if len(unknown) > 0 {
		out = append(out, l.T("Cannot verify selected VMs %s in Hyper-V cluster %s: participating node inventory is unavailable or out of date.", strings.Join(unknown, ", "), g.Options.HVCluster))
	}
	return out
}

func missingClusterVMAttention(l *language, groups []JobGroup, agents []Agent, now time.Time) []Attention {
	if !slices.ContainsFunc(groups, monitorsClusterVMs) {
		return nil
	}
	inventories := clusterVMInventories(agents, now)
	var out []Attention
	for _, g := range groups {
		observed := observeClusterVMs(g, inventories)
		for _, text := range clusterVMWarnings(l, g, observed) {
			out = append(out, Attention{"warning", g.Name, text, fmt.Sprintf("/job-groups/%d", g.ID), ""})
		}
	}
	return out
}
