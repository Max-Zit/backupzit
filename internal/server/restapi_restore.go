package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// apiRestoreRequest is the body of POST /api/v1/runs/{id}/restore: files
// of a backup restored by an agent, like the Restore button of a run.
type apiRestoreRequest struct {
	// AgentID is the agent that writes the files (0 = the backed-up one).
	AgentID int64 `json:"agent_id"`
	// Target is a folder on that agent; empty with Original restores the
	// files to where they were backed up from.
	Target   string   `json:"target"`
	Original bool     `json:"original"`
	Paths    []string `json:"paths"` // files and folders to restore; none = all
	Verify   bool     `json:"verify"`
}

func (s *Server) registerAPIRestore(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/runs/{id}/restore", s.api(PermRestore, func(w http.ResponseWriter, r *http.Request) (any, error) {
		id, err := pathInt(r, "id")
		if err != nil {
			return nil, err
		}
		var req apiRestoreRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			return nil, apiErr(http.StatusBadRequest, "invalid JSON body: %v", err)
		}
		req.Target = strings.TrimSpace(req.Target)
		switch {
		case req.Original && req.Target != "":
			return nil, apiErr(http.StatusBadRequest, "give either target or original, not both")
		case !req.Original && req.Target == "":
			return nil, apiErr(http.StatusBadRequest, "give a target folder, or original: true to restore to the original location")
		}
		b, err := s.store.GetRun(r.Context(), id)
		if err != nil {
			return nil, err
		}
		if b.Expired {
			return nil, apiErr(http.StatusConflict, "backup run %d was removed by the retention policy", id)
		}
		if req.AgentID == 0 {
			req.AgentID = b.AgentID
		}
		var paths []string
		for _, p := range req.Paths {
			if p = strings.TrimSpace(p); p != "" {
				paths = append(paths, p)
			}
		}
		rid, err := s.store.QueueRestore(r.Context(), id, req.AgentID, req.Target, paths, req.Verify)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, err
			}
			return nil, apiErr(http.StatusConflict, "%s", err.Error())
		}
		where := req.Target
		if req.Original {
			where = "the original location"
		}
		s.audit(r, "api.restore", "run #%d from backup #%d to %q on agent %d", rid, id, where, req.AgentID)
		return map[string]int64{"run_id": rid}, nil
	}))
}
