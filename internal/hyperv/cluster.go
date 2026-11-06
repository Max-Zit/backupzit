package hyperv

import "errors"

// ErrNoneHere is returned by Backup with Cluster set when none of the job's
// VMs runs on this node now: the other nodes back them up.
var ErrNoneHere = errors.New("none of the job's VMs runs on this node now")

// ErrNotAppConsistent is returned by Backup with RequireAppConsistent when
// a running VM could not get an application-consistent checkpoint: it was
// not backed up (the snapshot, if any, holds the other VMs).
var ErrNotAppConsistent = errors.New("no application-consistent checkpoint")

// pick chooses the VMs to back up from those on this host (by VMIDFor
// number) for a job selecting ids (nil = all) without excl. On a cluster
// node (cluster set) "all" means the clustered VMs only, and selected VMs
// that are not here are not missing: another node owns them now.
func pick(here []hostVM, ids, excl []int, cluster bool) (sel []int, missing []int) {
	if ids == nil {
		skip := map[int]bool{}
		for _, id := range excl {
			skip[id] = true
		}
		for _, vm := range here {
			if !skip[vm.ID] && (!cluster || vm.Clustered) {
				sel = append(sel, vm.ID)
			}
		}
		return sel, nil
	}
	have := map[int]bool{}
	for _, vm := range here {
		have[vm.ID] = true
	}
	for _, id := range ids {
		switch {
		case have[id]:
			sel = append(sel, id)
		case !cluster:
			missing = append(missing, id)
		}
	}
	return sel, missing
}

// hostVM is a VM of this host as pick sees it.
type hostVM struct {
	ID        int
	Clustered bool
}
