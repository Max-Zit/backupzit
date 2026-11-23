package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/update"
)

// QueueAgentUpdate queues the installation of an agent package. The run
// keeps the installer name and version in its paths.
func (s *Store) QueueAgentUpdate(ctx context.Context, agentID int64, u agentUpdate) (int64, error) {
	var id int64
	err := s.db.QueryRow(ctx, `INSERT INTO runs(agent_id, kind, trigger, repo_url, paths)
		SELECT $1,'agent-update','manual','',$2
		WHERE NOT EXISTS (SELECT 1 FROM runs WHERE agent_id=$1 AND kind='agent-update' AND status IN ('queued','running'))
		RETURNING id`, agentID, []string{u.File, u.Version}).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errors.New("an update of this agent is already pending")
	}
	return id, err
}

func (s *Server) addUpdate(run *Run, ar *api.Run) error {
	if len(run.Paths) != 2 || s.DistDir == "" {
		return errors.New("update installer missing")
	}
	sum, err := s.fileSHA256(filepath.Join(s.DistDir, filepath.Base(run.Paths[0])))
	if err != nil {
		return fmt.Errorf("installer %s: %w", run.Paths[0], err)
	}
	ar.UpdateFile, ar.UpdateVersion, ar.UpdateSHA256 = filepath.Base(run.Paths[0]), run.Paths[1], sum
	// The signed list of the installers of that version, from the server
	// package (GHSA-v284-8576-jp84): agents 0.56 and newer install only
	// what it lists. Checked here too, so a changed installer fails at once
	// with a clear message instead of on the agent.
	mpath := filepath.Join(s.DistDir, update.AgentManifestName(ar.UpdateVersion))
	raw, err := os.ReadFile(mpath) // #nosec G304 -- a file of the console's own installer folder
	if errors.Is(err, os.ErrNotExist) {
		return nil // older consoles' installers; agents 0.56+ refuse them
	}
	if err != nil {
		return err
	}
	sig, err := os.ReadFile(mpath + ".sig") // #nosec G304 -- a file of the console's own installer folder
	if err != nil {
		return fmt.Errorf("the signature of the list of agent installers is missing: %w", err)
	}
	f, err := update.VerifyAgentInstaller(raw, string(sig), nil, ar.UpdateVersion, ar.UpdateFile)
	if err != nil {
		return fmt.Errorf("agent installer %s: %w", ar.UpdateFile, err)
	}
	if !strings.EqualFold(f.SHA256, sum) {
		return fmt.Errorf("agent installer %s on the console is not the one of release %s (checksum mismatch): it was changed or damaged; install the console package again", ar.UpdateFile, ar.UpdateVersion)
	}
	ar.UpdateManifest, ar.UpdateManifestSig = string(raw), strings.TrimSpace(string(sig))
	return nil
}

// handleAgentDownload serves agent installers to enrolled agents.
func (s *Server) handleAgentDownload(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if s.DistDir == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") || fileKind(name) == "" {
		writeAPIError(w, http.StatusNotFound, "no such installer")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, filepath.Join(s.DistDir, name))
}

func (s *Server) handleAgentUpdate(w http.ResponseWriter, r *http.Request, _ string) {
	id, err := pathID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	a, err := s.store.GetAgent(r.Context(), id)
	if err != nil {
		redirectErr(w, r, "/agents", errors.New("unknown agent"))
		return
	}
	u, ok := s.availableUpdates([]Agent{a})[a.ID]
	if !ok {
		redirectErr(w, r, "/agents", fmt.Errorf("no newer agent installer for %s", a.Hostname))
		return
	}
	if _, err := s.store.QueueAgentUpdate(r.Context(), a.ID, u); err != nil {
		redirectErr(w, r, "/agents", err)
		return
	}
	s.audit(r, "agent.update", "%s from %s to %s", a.Hostname, a.Version, u.Version)
	redirectMsg(w, r, "/agents", fmt.Sprintf("Update of %s to %s queued. The agent installs it when it is idle and restarts.", a.Hostname, u.Version))
}

func (s *Server) handleAgentUpdateAll(w http.ResponseWriter, r *http.Request, _ string) {
	agents, err := s.store.ListAgents(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	n := 0
	for id, u := range s.availableUpdates(agents) {
		if _, err := s.store.QueueAgentUpdate(r.Context(), id, u); err == nil {
			n++
		}
	}
	s.audit(r, "agent.update", "%d agents", n)
	redirectMsg(w, r, "/agents", fmt.Sprintf("Updates queued for %d agents.", n))
}

// handleAgentMaxRuns serves POST /agents/{id}/max-runs.
func (s *Server) handleAgentMaxRuns(w http.ResponseWriter, r *http.Request, _ string) {
	id, _ := pathID(r)
	n, _ := strconv.Atoi(r.FormValue("max_runs"))
	a, err := s.store.GetAgent(r.Context(), id)
	if err != nil {
		redirectErr(w, r, "/agents", errors.New("unknown agent"))
		return
	}
	if !a.Parallel() && n > 1 {
		redirectErr(w, r, "/agents", fmt.Errorf("the agent on %s (%s) runs one job at a time; update it to %s or newer", a.Hostname, a.Version, minParallelVersion))
		return
	}
	if err := s.store.SetAgentMaxRuns(r.Context(), id, n); err != nil {
		redirectErr(w, r, "/agents", err)
		return
	}
	s.audit(r, "agent.max_runs", "%s: %d at once", a.Hostname, n)
	redirectMsg(w, r, "/agents", "Settings saved.")
}
