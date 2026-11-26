package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/anomaly"
	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/imaging"
	"github.com/max-zit/backupzit/internal/update"
)

type ctxKey int

const agentKey ctxKey = 1

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
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
	what := "agent"
	if req.Recovery {
		what = "recovery media"
	}
	// A new machine got access to the console: worth an audit entry.
	s.auditAs(r, "", "agent.enroll", fmt.Sprintf("%s %s enrolled (%s %s, agent %s)", what, req.Hostname, req.OS, req.Arch, req.Version))
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request) {
	a := agentFrom(r)
	var req api.PollRequest
	if err := readJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if err := s.store.TouchAgent(r.Context(), a.ID, req, remoteIP(r)); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	if len(req.Progress) > 0 {
		s.live.set(a.ID, req.Progress, s.clock())
	}
	if req.FullDiskAccess != "" {
		s.agentFullDiskAccess(r.Context(), a, req.FullDiskAccess, remoteIP(r))
	}
	s.clocks.seen(a.ID, req.Time, s.clock())
	resp := api.PollResponse{PollIntervalSec: s.PollInterval}
	if _, ok := s.inventoryWanted.LoadAndDelete(a.ID); ok {
		resp.SendInventory = true
	}
	// Storage reads wait for this agent (browsing storage reachable by
	// agents only, #63).
	resp.Relay = s.relay.pending(a.ID)
	if req.WantStatus {
		if st, err := s.agentStatus(r.Context(), a.ID); err == nil {
			resp.Status = st
		}
	}
	// Runs wait while two machines use this enrollment (a running copy);
	// only agent updates go out, after which agents tell themselves apart.
	held := s.agentDuplicate(r.Context(), a, req.Instance, req.Hostname, req.IPs, remoteIP(r))
	// With two copies polling, one copy's list says nothing about the other.
	if req.ReportsRuns && !held {
		s.failOrphanedRuns(r.Context(), a, req.Runs)
	}
	// A busy agent may take another run if it can run several at once
	// (MaxRuns); agent updates wait until it is idle.
	free := !req.Busy || (req.Parallel && req.Running < max(a.MaxRuns, 1) && !held)
	if req.Busy {
		resp.Cancel = s.store.cancelRequests(r.Context(), a.ID)
	}
	if !free && req.Parallel && !held {
		// A check of a VMware host or a storage connection test takes
		// seconds: it does not wait behind the agent's backups (#51, #63).
		if run, err := s.store.claimProbe(r.Context(), a.ID); err == nil && run != nil {
			if ar, err := s.toAPIRun(r.Context(), run); err == nil {
				resp.Run = ar
			} else if s.store.FinishRun(r.Context(), a.ID, run.ID, api.RunResult{Status: api.StatusFailed, Message: err.Error()}) == nil && run.Kind == api.KindVMwareProbe {
				_ = s.store.applyVMwareProbe(r.Context(), *run, api.RunResult{Status: api.StatusFailed, Message: err.Error()})
			}
		}
	}
	if free {
		run, err := s.store.claimRunIdle(r.Context(), a.ID, held, !req.Busy)
		if err != nil {
			s.log.Error("claim run", "err", err)
			writeAPIError(w, http.StatusInternalServerError, "database error")
			return
		}
		if run != nil {
			ar, err := s.toAPIRun(r.Context(), run)
			if err == nil && ar.Repository.NeedsVMBlocks && !req.ConfigurableVMBlocks {
				err = errors.New("update the agent to use configurable VM block sizes")
			}
			if err != nil {
				if s.store.FinishRun(r.Context(), a.ID, run.ID, api.RunResult{Status: api.StatusFailed, Message: err.Error()}) == nil {
					s.chainAfterRun(r.Context(), run.ID)
				}
			} else {
				resp.Run = ar
				s.log.Info("run assigned", "run", run.ID, "kind", run.Kind, "agent", a.Hostname)
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) toAPIRun(ctx context.Context, run *Run) (*api.Run, error) {
	if run.Kind == api.KindAgentUpdate {
		ar := &api.Run{ID: run.ID, Kind: run.Kind}
		return ar, s.addUpdate(run, ar)
	}
	if run.Kind == api.KindAgentLog || run.Kind == api.KindAgentUninstall {
		return &api.Run{ID: run.ID, Kind: run.Kind}, nil
	}
	if run.Kind == api.KindLeftoverCleanup { // needs no storage
		return s.cleanupRun(ctx, run)
	}
	if run.Kind == api.KindVMwareProbe {
		ar := &api.Run{ID: run.ID, Kind: run.Kind}
		if len(run.Paths) == 2 && run.Paths[0] == fingerprintProbe {
			// Only the fingerprints, before the host is stored (B3).
			ar.VMware = &api.VMwareHost{Name: run.Paths[1], Address: run.Paths[1], FingerprintsOnly: true}
			return ar, nil
		}
		if err := s.addVMware(ctx, run, ar); err != nil {
			return nil, err
		}
		if ar.VMware == nil {
			return nil, errors.New("the VMware host was removed from the console")
		}
		return ar, nil
	}
	if run.Kind == api.KindVMReplicaStart { // needs no storage
		ar := &api.Run{ID: run.ID, Kind: run.Kind}
		return ar, s.addReplica(ctx, run, ar)
	}
	if run.TargetID == nil {
		return nil, errors.New("storage target was deleted")
	}
	t, err := s.store.GetTarget(ctx, *run.TargetID)
	if err != nil {
		return nil, errors.New("storage target was deleted")
	}
	ar := &api.Run{
		ID:         run.ID,
		Kind:       run.Kind,
		Repository: targetRepository(t, run.RepoURL),
	}
	if run.JobName != nil {
		ar.JobName = *run.JobName
	}
	switch run.Kind {
	case api.KindRepoScan:
		ar.Repository = targetRepository(t, t.URL)
	case api.KindTargetTest:
		// The storage and its credentials; the test opens no repository,
		// so the recovery key stays on the console.
		ar.Repository = targetRepository(t, t.URL)
		ar.Repository.Password = ""
	case api.KindRepoRepair:
		// Removing the affected backups keeps the ones VMs run from (instant
		// recovery) and stops while retention is paused after a suspicious
		// backup (checked again here: the hold may have started since).
		if slices.Contains(run.Paths, repairForgetFlag) && run.TargetID != nil && !s.store.repoRetentionHold(ctx, *run.TargetID, run.RepoURL) {
			if keep, err := s.store.instantSnapshots(ctx); err == nil {
				ar.RepairForget, ar.KeepSnapshots = true, keep
			}
		}
	case api.KindBackup:
		s.addRetention(ctx, run, ar)
		ar.Paths, ar.Excludes = run.Paths, run.Excludes
		if err := s.addNAS(ctx, run, ar); err != nil {
			return nil, err
		}
		if err := s.addM365(ctx, run, ar); err != nil {
			return nil, err
		}
	case api.KindRestore:
		ar.SnapshotID = run.SnapshotID
		ar.RestoreTarget = run.RestoreTarget
		ar.Includes = run.Paths
		ar.Verify = run.RestoreVerify
		if run.RestoreTarget == "" {
			if err := s.addNAS(ctx, run, ar); err != nil {
				return nil, err
			}
		}
	case api.KindImageBackup:
		s.addRetention(ctx, run, ar)
		if run.ImageDisk != nil {
			ar.ImageDisk = *run.ImageDisk
			// Older agents read "all disks" as a disk number.
			if *run.ImageDisk == imaging.AllDisks {
				if a, err := s.store.GetAgent(ctx, run.AgentID); err == nil && strings.Count(a.Version, ".") == 2 && update.Newer(minAllDisksVersion, a.Version) {
					return nil, fmt.Errorf("the agent on %s (%s) cannot back up all disks yet; update it to %s or newer (Agents page)", a.Hostname, a.Version, minAllDisksVersion)
				}
			}
		}
		ar.ImagePartitions = run.ImagePartitions
	case api.KindVMFileRestore:
		ar.SnapshotID = run.SnapshotID
		var o api.VMFileRestore
		if json.Unmarshal(run.VMRestore, &o) == nil {
			ar.VMFiles = &o
		}
	case api.KindSystemBackup:
		s.addRetention(ctx, run, ar)
		ar.Paths, ar.Excludes = run.Paths, run.Excludes
	case api.KindSystemRestore:
		ar.SnapshotID = run.SnapshotID
		ar.SystemRestore = systemRestoreOptions(*run)
	case api.KindVMBackup:
		s.addRetention(ctx, run, ar)
		ar.VMs, ar.VMExclude = run.Paths, run.Excludes
		if run.JobID != nil {
			if j, err := s.store.GetJob(ctx, *run.JobID); err == nil {
				ar.HVCluster = j.Options.HVCluster
				if j.Options.AppConsistent {
					if a, err := s.store.GetAgent(ctx, run.AgentID); err == nil && strings.Count(a.Version, ".") == 2 && update.Newer(minAppConsistentVersion, a.Version) {
						return nil, fmt.Errorf("the agent on %s (%s) cannot require application-consistent checkpoints yet; update it to %s or newer (Agents page)", a.Hostname, a.Version, minAppConsistentVersion)
					}
					ar.AppConsistent = true
				}
				if len(j.Options.ExcludeDisks) > 0 {
					if a, err := s.store.GetAgent(ctx, run.AgentID); err == nil && strings.Count(a.Version, ".") == 2 && update.Newer(minExcludeDisksVersion, a.Version) {
						return nil, fmt.Errorf("the agent on %s (%s) cannot exclude disks yet and would back up every disk; update it to %s or newer (Agents page)", a.Hostname, a.Version, minExcludeDisksVersion)
					}
					ar.VMExcludeDisks = j.Options.ExcludeDisks
				}
				ar.VMCrashConsistentOK = j.Options.CrashConsistentOK
				if j.Options.VMwareNoQuiesce {
					if a, err := s.store.GetAgent(ctx, run.AgentID); err == nil && strings.Count(a.Version, ".") == 2 && update.Newer(minNoQuiesceVersion, a.Version) {
						return nil, fmt.Errorf("the agent on %s (%s) cannot take snapshots without quiescing yet and would quiesce the guests; update it to %s or newer (Agents page)", a.Hostname, a.Version, minNoQuiesceVersion)
					}
					ar.VMwareNoQuiesce = true
				}
			}
		}
		if err := s.addVMware(ctx, run, ar); err != nil {
			return nil, err
		}
		if err := s.addPart(ctx, run, ar); err != nil {
			return nil, err
		}
	case api.KindSQLBackup, api.KindSQLLog:
		s.addRetention(ctx, run, ar)
		if err := s.addSQL(ctx, run, ar); err != nil {
			return nil, err
		}
	case api.KindSQLRestore:
		ar.SnapshotID = run.SnapshotID
		ar.SQL = sqlRestoreOptions(*run)
	case api.KindVMReplica, api.KindVMReplicaStart:
		if err := s.addReplica(ctx, run, ar); err != nil {
			return nil, err
		}
	case api.KindVMInstant, api.KindVMInstantFinish, api.KindVMInstantDiscard:
		ar.SnapshotID = run.SnapshotID
		ar.VMRestore = vmRestoreOptions(*run)
		if err := s.addVMware(ctx, run, ar); err != nil {
			return nil, err
		}
	case api.KindVMRestore:
		ar.SnapshotID = run.SnapshotID
		ar.VMRestore = vmRestoreOptions(*run)
		if err := s.addVMware(ctx, run, ar); err != nil {
			return nil, err
		}
	case api.KindImageFileRestore:
		ar.SnapshotID = run.SnapshotID
		ar.Includes = run.Paths
		ar.RestoreTarget = run.RestoreTarget
		if len(run.ImagePartitions) > 0 {
			ar.ImagePartition = run.ImagePartitions[0]
		}
	case api.KindVerify:
		ar.SnapshotID = run.SnapshotID
		s.addVerifyParams(ctx, *run, ar)
	case api.KindCopy:
		s.addRetention(ctx, run, ar)
		if err := s.addCopySource(ctx, run, ar); err != nil {
			return nil, err
		}
		if err := s.addCopyPart(ctx, run, ar); err != nil {
			return nil, err
		}
	case api.KindImageRestore:
		ar.SnapshotID = run.SnapshotID
		if run.TargetDisk != nil {
			ar.TargetDisk = *run.TargetDisk
		}
		ar.KeepOffline = run.KeepOffline
		ar.NewHardware, ar.DriverPath = run.NewHardware, run.DriverPath
		if len(run.ImagePartitions) > 0 {
			ar.ImageIndex = run.ImagePartitions[0] // which disk of the backup
		}
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
	if run, err := s.store.GetRun(r.Context(), id); err == nil && run.Kind == api.KindAgentUninstall {
		s.agentUninstalled(context.WithoutCancel(r.Context()), a, id, res)
		writeJSON(w, http.StatusOK, struct{}{})
		return
	} else if err == nil && run.Kind == api.KindVMwareProbe {
		// What the proxy saw of its VMware host (#51): no chains, no alerts.
		if err := s.store.applyVMwareProbe(context.WithoutCancel(r.Context()), run, res); err != nil {
			s.log.Error("store the VMware host check", "run", id, "err", err)
		}
		writeJSON(w, http.StatusOK, struct{}{})
		return
	} else if err == nil && run.Kind == api.KindTargetTest {
		// A storage test the user waits for on its page (#63): no chains,
		// no alerts; a failure goes to the console log.
		s.finishTargetTest(context.WithoutCancel(r.Context()), a, run, res)
		writeJSON(w, http.StatusOK, struct{}{})
		return
	}
	s.chainAfterRun(r.Context(), id)
	if run, err := s.store.GetRun(r.Context(), id); err == nil && run.JobID != nil && isBackupKind(run.Kind) {
		// A backup split over several proxies is copied once, after its last
		// part: chainAfterRun queues those copies when it decides the chain.
		if res.Status != api.StatusFailed && run.Batch == nil {
			if ids, err := s.store.QueueCopiesAfter(r.Context(), *run.JobID); err != nil {
				s.log.Error("queue copy jobs", "job", *run.JobID, "err", err)
			} else if len(ids) > 0 {
				s.log.Info("copy jobs queued after backup", "job", *run.JobID, "runs", ids)
			}
		}
		if res.Status != api.StatusFailed {
			if rid, err := s.store.QueueReplica(r.Context(), run); err != nil {
				s.log.Error("queue replication", "job", *run.JobID, "err", err)
			} else if rid != 0 {
				s.log.Info("replication queued after backup", "job", *run.JobID, "run", rid)
			}
		}
	}
	if s.Notifier != nil {
		go func() { // #nosec G118 -- background work that outlives the request on purpose
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			s.Notifier.Pass(ctx, time.Now())
		}()
	}
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
	j.Options.apply(ar, run.Kind)
	if j.RetentionHold {
		return // paused after a suspicious backup
	}
	if j.Kind == "copy" && j.SourceJobID != nil {
		// A copy job removes nothing while its source job is paused after a
		// suspicious backup: the damaged backups it copies must not push the
		// clean copies out.
		if src, err := s.store.GetJob(ctx, *j.SourceJobID); err != nil || src.RetentionHold {
			return
		}
	}
	if !j.Retention.Empty() {
		p := j.Retention
		ar.Retention = &p
		// VMs running from a backup (instant recovery) need it.
		if keep, err := s.store.instantSnapshots(ctx); err == nil {
			ar.KeepSnapshots = keep
		} else {
			ar.Retention = nil
		}
		// The agent compares the new backup with these before it applies
		// the retention (B5): a suspicious backup removes nothing.
		if ar.Retention != nil && anomaly.Checked(run.Kind) {
			if hist, err := s.store.recentStats(ctx, j.ID, run.ID, run.Kind, run.Part); err == nil && len(hist) >= anomaly.MinHistory {
				ar.RecentStats = hist
			}
		}
	}
}

// remoteIP is the address an agent's request comes from.
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// minAllDisksVersion is the first agent that images all disks in one job.
const minAllDisksVersion = "0.33.7"

// minAppConsistentVersion is the first agent that honours AppConsistent;
// older agents would quietly fall back to crash-consistent checkpoints.
const minAppConsistentVersion = "0.48.0"

// minNoQuiesceVersion is the first agent that honours VMwareNoQuiesce;
// older ones would still quiesce the guests.
const minNoQuiesceVersion = "0.58.0"

// minExcludeDisksVersion is the first agent that honours VMExcludeDisks;
// older ones would silently back up the excluded disks.
const minExcludeDisksVersion = "0.49.0"
