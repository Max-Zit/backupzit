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

func pveAgents(agents []Agent) []pveAgent {
	var out []pveAgent
	for _, a := range agents {
		if inv := a.PVE(); inv != nil {
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
