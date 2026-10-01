// Package api defines the JSON messages exchanged between agents and the
// management server.
//
// All agent endpoints live under /api/agent/ and, except enroll, require
// "Authorization: Bearer <agent-uuid>:<secret>".
package api

import (
	"encoding/json"
)

const (
	PathEnroll = "/api/agent/enroll"
	PathPoll   = "/api/agent/poll"
	// PathRunStart and PathRunFinish take the run id as a suffix: /api/agent/runs/{id}/start
	PathRunsPrefix = "/api/agent/runs/"
)

// EnrollRequest registers a new agent using a tenant enrollment token.
type EnrollRequest struct {
	Token    string `json:"token"`
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Version  string `json:"version"`
}

// EnrollResponse carries the agent's permanent credentials.
type EnrollResponse struct {
	AgentUUID string `json:"agent_uuid"`
	Secret    string `json:"secret"`
}

// PollRequest is the agent heartbeat.
type PollRequest struct {
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Version  string `json:"version"`
	// Busy is true while the agent is executing a run; the server then
	// assigns no new work.
	Busy bool `json:"busy"`
}

// PollResponse returns work for the agent.
type PollResponse struct {
	Run             *Run `json:"run,omitempty"`
	PollIntervalSec int  `json:"poll_interval_sec"`
}

// Run kinds.
const (
	KindBackup  = "backup"
	KindRestore = "restore"
)

// Run is a unit of work assigned to an agent.
type Run struct {
	ID         int64      `json:"id"`
	Kind       string     `json:"kind"`
	JobName    string     `json:"job_name,omitempty"`
	Repository Repository `json:"repository"`

	// Backup
	Paths    []string `json:"paths,omitempty"`
	Excludes []string `json:"excludes,omitempty"`

	// Restore
	SnapshotID    string   `json:"snapshot_id,omitempty"`
	RestoreTarget string   `json:"restore_target,omitempty"` // "" = original location
	Includes      []string `json:"includes,omitempty"`
	Verify        bool     `json:"verify,omitempty"`
}

// Repository tells the agent where and how to store data.
type Repository struct {
	URL          string `json:"url"`
	SFTPPassword string `json:"sftp_password,omitempty"`
	SFTPKey      string `json:"sftp_key,omitempty"` // PEM
	SFTPHostKey  string `json:"sftp_host_key,omitempty"`
}

// Run statuses.
const (
	StatusQueued  = "queued"
	StatusRunning = "running"
	StatusSuccess = "success"
	StatusWarning = "warning" // finished, but some files had errors
	StatusFailed  = "failed"
)

// RunResult is reported by the agent when a run finishes.
type RunResult struct {
	Status     string          `json:"status"`
	SnapshotID string          `json:"snapshot_id,omitempty"`
	Stats      json.RawMessage `json:"stats,omitempty"`
	Errors     []string        `json:"errors,omitempty"`
	Message    string          `json:"message,omitempty"`
}

// Error is returned by the server with non-2xx responses.
type Error struct {
	Error string `json:"error"`
}
