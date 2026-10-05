package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/pve"
)

// pveInventories maps agent IDs to the guests of their node, for the job
// form.
func pveInventories(agents []Agent) map[string][]pve.Guest {
	out := map[string][]pve.Guest{}
	for _, a := range agents {
		if a.PVE() != nil {
			gs := a.LocalGuests()
			if gs == nil {
				gs = []pve.Guest{}
			}
			out[strconv.FormatInt(a.ID, 10)] = gs
		}
	}
	return out
}

// pveAgent is an agent on a Proxmox node, for the restore form.
type pveAgent struct {
	Agent
	Inv *pve.Inventory
}

func pveAgents(agents []Agent) []pveAgent { return hypervisorAgents(agents, "proxmox") }

// hypervisorAgents are the agents on hosts of the given platform.
func hypervisorAgents(agents []Agent, platform string) []pveAgent {
	var out []pveAgent
	for _, a := range agents {
		if inv := a.PVE(); inv != nil && a.Platform() == platform {
			out = append(out, pveAgent{a, inv})
		}
	}
	return out
}

func (s *Server) handleVMRestore(w http.ResponseWriter, r *http.Request, runID int64, back string) {
	vmid, err := strconv.Atoi(r.FormValue("vmid"))
	if err != nil {
		redirectErr(w, r, back, errors.New("choose the VM or container to restore"))
		return
	}
	o := api.VMRestore{VMID: vmid, Name: r.FormValue("name"), Storage: r.FormValue("storage"),
		Start: r.FormValue("start") == "on"}
	switch r.FormValue("id_mode") {
	case "original":
		o.Overwrite = r.FormValue("overwrite") == "on"
	case "next":
		o.NewVMID = -1
	case "instant":
		o.NewVMID = -1
		var rid int64
		var err error
		if hid := formID(r, "vmware_host_id"); hid > 0 {
			rid, err = s.store.QueueVMwareInstant(r.Context(), runID, hid, o)
		} else {
			rid, err = s.store.QueueVMInstant(r.Context(), runID, formID(r, "agent_id"), o)
		}
		if err != nil {
			redirectErr(w, r, back, err)
			return
		}
		s.audit(r, "restore.vm-instant", "run #%d: guest %d runs from backup #%d", rid, vmid, runID)
		redirectMsg(w, r, fmt.Sprintf("/runs/%d", rid), "Instant recovery queued.")
		return
	case "custom":
		n, err := strconv.Atoi(strings.TrimSpace(r.FormValue("new_vmid")))
		if err != nil {
			redirectErr(w, r, back, errors.New("enter the ID for the restored guest"))
			return
		}
		o.NewVMID = n
		if n == vmid {
			o.NewVMID = 0
			o.Overwrite = r.FormValue("overwrite") == "on"
		}
	default:
		redirectErr(w, r, back, errors.New("choose the ID of the restored guest"))
		return
	}
	var rid int64
	if hid := formID(r, "vmware_host_id"); hid > 0 {
		rid, err = s.store.QueueVMwareRestore(r.Context(), runID, hid, o)
	} else {
		rid, err = s.store.QueueVMRestore(r.Context(), runID, formID(r, "agent_id"), o)
	}
	if err != nil {
		redirectErr(w, r, back, err)
		return
	}
	s.audit(r, "restore.vm", "run #%d: guest %d from backup #%d (new id %d, overwrite %v)", rid, vmid, runID, o.NewVMID, o.Overwrite)
	redirectMsg(w, r, fmt.Sprintf("/runs/%d", rid), "VM restore queued.")
}

func (s *Server) handleSystemRestore(w http.ResponseWriter, r *http.Request, runID int64, back string) {
	o := api.SystemRestore{Mode: r.FormValue("mode")}
	agentID := formID(r, "agent_id")
	switch o.Mode {
	case "disk":
		if r.FormValue("confirm_erase") != "on" {
			redirectErr(w, r, back, errors.New("confirm that the target disk will be erased"))
			return
		}
		o.Device = r.FormValue("device")
		o.NewHardware = r.FormValue("new_hardware") == "on"
		agentID = formID(r, "disk_agent_id")
	case "pve-vm":
		o.Storage, o.Name, o.Bridge = r.FormValue("storage"), r.FormValue("name"), strings.TrimSpace(r.FormValue("bridge"))
		o.VMID = -1
		if v := strings.TrimSpace(r.FormValue("vmid")); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				redirectErr(w, r, back, errors.New("invalid VM ID"))
				return
			}
			o.VMID = n
		}
		o.Memory = atoiDefault(r.FormValue("memory"))
		o.Cores = atoiDefault(r.FormValue("cores"))
		o.Start = r.FormValue("start") == "on"
		o.NewHardware = true
		agentID = formID(r, "pve_agent_id")
	}
	rid, err := s.store.QueueSystemRestore(r.Context(), runID, agentID, o)
	if err != nil {
		redirectErr(w, r, back, err)
		return
	}
	s.audit(r, "restore.system", "run #%d from backup #%d (%s %s)", rid, runID, o.Mode, o.Device+o.Storage)
	redirectMsg(w, r, fmt.Sprintf("/runs/%d", rid), "System restore queued.")
}

// linuxAgents are the agents that can restore a Linux system to a disk.
func linuxAgents(agents []Agent) []Agent {
	var out []Agent
	for _, a := range agents {
		if isLinuxAgent(a) {
			out = append(out, a)
		}
	}
	return out
}

// instantStorage lists the storage of the node of a vm-instant run that
// can hold its disks when it is finished.
func (s *Server) instantStorage(ctx context.Context, run Run) []string {
	if runInstant(run) == nil {
		return nil
	}
	if h, err := s.store.vmwareHostOfRun(ctx, run.ID); err == nil && h != nil {
		var out []string
		if inv := h.Inv(); inv != nil {
			for _, ds := range inv.Storage {
				if !strings.HasPrefix(ds, "backupzit-") {
					out = append(out, ds)
				}
			}
		}
		return out
	}
	a, err := s.store.GetAgent(ctx, run.AgentID)
	if err != nil || a.PVE() == nil {
		return nil
	}
	var out []string
	for _, st := range a.PVE().Storage {
		if st != pve.InstantStorage {
			out = append(out, st)
		}
	}
	return out
}

// handleInstantEnd serves POST /runs/{id}/instant: finish or discard the VM
// a vm-instant run started.
func (s *Server) handleInstantEnd(w http.ResponseWriter, r *http.Request, _ string) {
	id, _ := pathID(r)
	back := fmt.Sprintf("/runs/%d", id)
	finish := r.FormValue("action") == "finish"
	rid, err := s.store.QueueInstantEnd(r.Context(), id, finish, r.FormValue("storage"))
	if err != nil {
		redirectErr(w, r, back, err)
		return
	}
	what := "discard"
	if finish {
		what = "finish to storage " + r.FormValue("storage")
	}
	s.audit(r, "restore.vm-instant-end", "run #%d: %s the VM of run #%d", rid, what, id)
	redirectMsg(w, r, fmt.Sprintf("/runs/%d", rid), "Queued.")
}

// jobGuests are the VMs a VM job can select, for its edit form.
func (s *Server) jobGuests(ctx context.Context, j Job) []pve.Guest {
	if j.Kind != JobVM {
		return nil
	}
	if j.VMwareHostID != nil {
		h, err := s.store.GetVMwareHost(ctx, *j.VMwareHostID)
		if err != nil || h.Inv() == nil {
			return nil
		}
		return h.Inv().Guests
	}
	a, err := s.store.GetAgent(ctx, j.AgentID)
	if err != nil {
		return nil
	}
	return a.LocalGuests()
}
