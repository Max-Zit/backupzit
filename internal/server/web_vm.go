package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/backupzit/backupzit/internal/api"
	"github.com/backupzit/backupzit/internal/pve"
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
	rid, err := s.store.QueueVMRestore(r.Context(), runID, formID(r, "agent_id"), o)
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
