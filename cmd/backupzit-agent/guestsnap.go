package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/max-zit/backupzit/internal/repo"
)

// latestNote receives the line naming the snapshot "latest" resolved to.
var latestNote io.Writer = os.Stderr

// loadGuestSnapshot loads the snapshot that ref names. "latest" means the
// newest snapshot holding guest vmid on platform ("" = any platform,
// "proxmox" also matches older snapshots without a platform), not the
// newest of the repository: with VMs backed up one by one into the same
// repository, the newest snapshot may hold another VM. The platform is the
// one the command's restore accepts, so both find the same guest.
//
// Guest IDs are not unique across hosts (every Proxmox VE install starts at
// 100), so "latest" names the snapshot it chose, and refuses when the guest
// ID belongs to differently named guests of several hosts: then the
// snapshot ID must be given.
func loadGuestSnapshot(ctx context.Context, r *repo.Repository, ref, platform string, vmid int) (*repo.Snapshot, error) {
	if ref != "latest" {
		return r.LoadSnapshot(ctx, ref)
	}
	sns, err := r.ListSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	if len(sns) == 0 {
		return nil, errors.New("repository has no snapshots")
	}
	var found *repo.Snapshot
	var foundName string
	names := map[string]map[string]bool{} // host -> guest names with that ID
	for i := len(sns) - 1; i >= 0; i-- {
		for _, g := range sns[i].Guests {
			if g.VMID != vmid || !platformMatches(platform, g.Platform) {
				continue
			}
			if found == nil {
				found, foundName = sns[i], g.Name
			}
			if names[sns[i].Hostname] == nil {
				names[sns[i].Hostname] = map[string]bool{}
			}
			names[sns[i].Hostname][g.Name] = true
		}
	}
	if found == nil {
		where := ""
		if platform != "" {
			where = " (" + platform + ")"
		}
		return nil, fmt.Errorf("no snapshot in the repository contains guest %d%s", vmid, where)
	}
	if len(names) > 1 {
		all := map[string]bool{}
		var hosts []string
		for h, ns := range names {
			for n := range ns {
				all[n] = true
			}
			hosts = append(hosts, h)
		}
		if len(all) > 1 {
			sort.Strings(hosts)
			return nil, fmt.Errorf("guest %d is in backups of several hosts (%s) under different names; give the snapshot ID instead of latest (see the snapshots command)", vmid, strings.Join(hosts, ", "))
		}
	}
	fmt.Fprintf(latestNote, "latest: snapshot %s of %s from %s, guest %d %q\n",
		found.ID.Short(), found.Hostname, found.Time.Local().Format("2006-01-02 15:04"), vmid, foundName)
	return found, nil
}

func platformMatches(want, have string) bool {
	return want == "" || have == want || (want == "proxmox" && have == "")
}
