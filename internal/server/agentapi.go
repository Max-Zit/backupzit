package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/backupzit/backupzit/internal/api"
)

type ctxKey int

const agentKey ctxKey = 1

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeAPIError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, api.Error{Error: msg})
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<20))
	return dec.Decode(v)
}

// agentAuth authenticates "Authorization: Bearer <uuid>:<secret>".
func (s *Server) agentAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		uuid, secret, ok := strings.Cut(auth, ":")
		if !ok {
			writeAPIError(w, http.StatusUnauthorized, "missing agent credentials")
			return
		}
		a, err := s.store.AuthenticateAgent(r.Context(), uuid, secret)
		if err != nil {
			writeAPIError(w, http.StatusUnauthorized, err.Error())
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), agentKey, a)))
	}
}

func agentFrom(r *http.Request) Agent { return r.Context().Value(agentKey).(Agent) }

func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req api.EnrollRequest
	if err := readJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request")
		return
	}
	resp, err := s.store.EnrollAgent(r.Context(), req)
	if err != nil {
		writeAPIError(w, http.StatusForbidden, err.Error())
		return
	}
	s.log.Info("agent enrolled", "hostname", req.Hostname, "uuid", resp.AgentUUID)
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request) {
	a := agentFrom(r)
	var req api.PollRequest
	if err := readJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if err := s.store.TouchAgent(r.Context(), a.ID, req); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	resp := api.PollResponse{PollIntervalSec: s.PollInterval}
	if !req.Busy {
		run, err := s.store.ClaimRun(r.Context(), a.ID)
		if err != nil {
			s.log.Error("claim run", "err", err)
			writeAPIError(w, http.StatusInternalServerError, "database error")
			return
		}
		if run != nil {
			ar, err := s.toAPIRun(r.Context(), run)
			if err != nil {
				s.store.FinishRun(r.Context(), a.ID, run.ID, api.RunResult{Status: api.StatusFailed, Message: err.Error()})
			} else {
				resp.Run = ar
				s.log.Info("run assigned", "run", run.ID, "kind", run.Kind, "agent", a.Hostname)
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) toAPIRun(ctx context.Context, run *Run) (*api.Run, error) {
	if run.TargetID == nil {
		return nil, errors.New("storage target was deleted")
	}
	t, err := s.store.GetTarget(ctx, *run.TargetID)
	if err != nil {
		return nil, errors.New("storage target was deleted")
	}
	ar := &api.Run{
		ID:   run.ID,
		Kind: run.Kind,
		Repository: api.Repository{
			URL:          run.RepoURL,
			SFTPPassword: t.SFTPPassword,
			SFTPKey:      t.SFTPKey,
			SFTPHostKey:  t.SFTPHostKey,
			S3AccessKey:  t.S3AccessKey,
			S3SecretKey:  t.S3SecretKey,
			S3Region:     t.S3Region,
			SMBPassword:  t.SMBPassword,
			SMBDomain:    t.SMBDomain,
		},
	}
	if run.JobName != nil {
		ar.JobName = *run.JobName
	}
	switch run.Kind {
	case api.KindBackup:
		s.addRetention(ctx, run, ar)
		ar.Paths, ar.Excludes = run.Paths, run.Excludes
	case api.KindRestore:
		ar.SnapshotID = run.SnapshotID
		ar.RestoreTarget = run.RestoreTarget
		ar.Includes = run.Paths
		ar.Verify = run.RestoreVerify
	case api.KindImageBackup:
		s.addRetention(ctx, run, ar)
		if run.ImageDisk != nil {
			ar.ImageDisk = *run.ImageDisk
		}
		ar.ImagePartitions = run.ImagePartitions
	case api.KindImageFileRestore:
		ar.SnapshotID = run.SnapshotID
		ar.Includes = run.Paths
		ar.RestoreTarget = run.RestoreTarget
		if len(run.ImagePartitions) > 0 {
			ar.ImagePartition = run.ImagePartitions[0]
		}
	case api.KindImageRestore:
		ar.SnapshotID = run.SnapshotID
		if run.TargetDisk != nil {
			ar.TargetDisk = *run.TargetDisk
		}
		ar.KeepOffline = run.KeepOffline
	}
	return ar, nil
}

// handleRunFinish serves POST /api/agent/runs/{id}/finish.
func (s *Server) handleRunFinish(w http.ResponseWriter, r *http.Request) {
	a := agentFrom(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid run id")
		return
	}
	var res api.RunResult
	if err := readJSON(r, &res); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if err := s.store.FinishRun(r.Context(), a.ID, id, res); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "run not found or not running")
			return
		}
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.log.Info("run finished", "run", id, "status", res.Status, "agent", a.Hostname)
	writeJSON(w, http.StatusOK, struct{}{})
}

// addRetention attaches the job's retention policy to a backup run.
func (s *Server) addRetention(ctx context.Context, run *Run, ar *api.Run) {
	if run.JobID == nil {
		return
	}
	j, err := s.store.GetJob(ctx, *run.JobID)
	if err != nil {
		return
	}
	ar.JobID = j.ID
	if !j.Retention.Empty() {
		p := j.Retention
		ar.Retention = &p
	}
}
