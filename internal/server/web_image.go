package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/backupzit/backupzit/internal/imaging"
	"github.com/backupzit/backupzit/internal/repo"
)

// inventories maps agent ID to its reported disks, for the disk pickers.
func inventories(agents []Agent) map[string][]imaging.Disk {
	out := map[string][]imaging.Disk{}
	for _, a := range agents {
		if d := a.Disks(); len(d) > 0 {
			out[strconv.FormatInt(a.ID, 10)] = d
		}
	}
	return out
}

// partitionSelection reads ticked partitions. Ticking every partition of
// the disk means "whole disk" (nil), so partitions added later are
// included too.
func (s *Server) partitionSelection(r *http.Request, agentID int64, disk int) []int {
	var sel []int
	for _, v := range r.Form["image_parts"] {
		if n, err := strconv.Atoi(v); err == nil {
			sel = append(sel, n)
		}
	}
	if len(sel) == 0 {
		return nil
	}
	if a, err := s.store.GetAgent(r.Context(), agentID); err == nil {
		for _, d := range a.Disks() {
			if d.Number == disk && len(d.Partitions) == len(sel) {
				return nil
			}
		}
	}
	return sel
}

// imageDetails decodes the disk layout reported by an image backup run.
func imageDetails(run Run) *repo.DiskImage {
	if len(run.Details) == 0 {
		return nil
	}
	var img repo.DiskImage
	if json.Unmarshal(run.Details, &img) != nil {
		return nil
	}
	return &img
}

func (s *Server) handleImageRestore(w http.ResponseWriter, r *http.Request, runID int64, back string) {
	if r.FormValue("confirm_erase") != "on" {
		redirectErr(w, r, back, errors.New("confirm that the target disk will be erased"))
		return
	}
	disk, err := strconv.Atoi(r.FormValue("target_disk"))
	if err != nil {
		redirectErr(w, r, back, errors.New("choose a target disk"))
		return
	}
	rid, err := s.store.QueueImageRestore(r.Context(), runID, formID(r, "agent_id"), disk, r.FormValue("keep_offline") == "on")
	if err != nil {
		redirectErr(w, r, back, err)
		return
	}
	redirectMsg(w, r, fmt.Sprintf("/runs/%d", rid), "Image restore queued.")
}

// atoiDefault parses a non-negative form number; anything else is 0.
func atoiDefault(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
