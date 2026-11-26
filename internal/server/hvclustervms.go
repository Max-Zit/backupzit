package server

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Selected VMs that a Hyper-V cluster no longer runs (0.62, #72). A cluster
// job that names its VMs backs up those its nodes report as clustered; a VM
// removed from the cluster (deleted, or no longer a role) was skipped
// without a word. The dashboard and the job page now name it, but only
// when every node of the job reported its VMs recently: a node that is
// offline, or busy and silent, makes the state "cannot check" instead of a
// false "missing".

// clusterInventoryAge is how old a node's VM list may be for the check
// (nodes report it every 15 minutes).
const clusterInventoryAge = 20 * time.Minute

// clusterVMCheck is the result for one cluster job.
type clusterVMCheck struct {
	Missing []string // "102 (web)" or "102"
	Unknown []string // VM numbers that cannot be checked now
}

// checkClusterVMs compares the VMs g selected with the clustered VMs its
// nodes report.
func checkClusterVMs(g JobGroup, agents []Agent, now time.Time) clusterVMCheck {
	var res clusterVMCheck
	if !g.Enabled || g.Kind != JobVM || g.Scope != GroupHVCluster || len(g.Paths) == 0 || slices.Contains(g.Paths, "*") {
		return res
	}
	var ids []int
	for _, p := range g.Paths {
		if id, err := strconv.Atoi(strings.TrimSpace(p)); err == nil {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	clustered, names := map[int]bool{}, map[int]string{}
	nodes, complete := 0, true
	for _, a := range agents {
		if !g.Matches(a) {
			continue
		}
		nodes++
		inv := a.PVE()
		if inv == nil || inv.Guests == nil || !a.Online() || a.HypervisorAt == nil || now.Sub(*a.HypervisorAt) > clusterInventoryAge {
			complete = false
			continue
		}
		for _, vm := range inv.Guests {
			if vm.Name != "" {
				names[vm.VMID] = clip(strings.Join(strings.Fields(vm.Name), " "), 100)
			}
			if vm.Clustered {
				clustered[vm.VMID] = true
			}
		}
	}
	for _, id := range ids {
		switch {
		case clustered[id]:
		case complete && nodes > 0:
			label := strconv.Itoa(id)
			if n := names[id]; n != "" {
				label += " (" + n + ")"
			}
			res.Missing = append(res.Missing, label)
		default:
			res.Unknown = append(res.Unknown, strconv.Itoa(id))
		}
	}
	return res
}

// clusterVMTexts are the warnings of a check, in l.
func clusterVMTexts(l *language, g JobGroup, c clusterVMCheck) []string {
	var out []string
	for _, vm := range c.Missing {
		out = append(out, l.T("VM %s is no longer a clustered VM of %s, so the job does not back it up", vm, g.Options.HVCluster))
	}
	if len(c.Unknown) > 0 {
		out = append(out, l.T("cannot check VMs %s of %s now: a node is offline or has not reported its VMs for 20 minutes", strings.Join(c.Unknown, ", "), g.Options.HVCluster))
	}
	return out
}

// clusterVMAttention are the dashboard entries of all cluster jobs.
func (s *Server) clusterVMAttention(ctx context.Context, l *language, agents []Agent, now time.Time) []Attention {
	groups, err := s.store.ListJobGroups(ctx)
	if err != nil {
		return nil
	}
	var out []Attention
	for _, g := range groups {
		for _, text := range clusterVMTexts(l, g, checkClusterVMs(g, agents, now)) {
			out = append(out, Attention{"warning", g.Name, text, fmt.Sprintf("/job-groups/%d", g.ID), ""})
		}
	}
	return out
}
