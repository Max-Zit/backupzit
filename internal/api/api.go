// Package api defines the JSON messages exchanged between agents and the
// management server.
//
// All agent endpoints live under /api/agent/ and, except enroll, require
// "Authorization: Bearer <agent-uuid>:<secret>".
package api

import (
	"encoding/json"

	"github.com/backupzit/backupzit/internal/repo"
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
	// Recovery marks a temporary agent running from recovery media.
	Recovery bool `json:"recovery,omitempty"`
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
	// Disks is the disk inventory (JSON array of imaging.Disk). Agents send
	// it on start and periodically; nil means "unchanged / not included".
	Disks json.RawMessage `json:"disks,omitempty"`
}

// PollResponse returns work for the agent.
type PollResponse struct {
	Run             *Run `json:"run,omitempty"`
	PollIntervalSec int  `json:"poll_interval_sec"`
}

// Run kinds.
const (
	KindBackup       = "backup"        // files and folders
	KindRestore      = "restore"       // files and folders
	KindImageBackup  = "image-backup"  // whole disk or partitions
	KindImageRestore = "image-restore" // image onto a disk
	// KindImageFileRestore extracts files and folders from an image backup.
	KindImageFileRestore = "image-file-restore"
	// KindCopy copies the backups of a job to a second storage target.
	KindCopy = "copy"
	// KindVerify restores a random sample of a backup to prove it is restorable.
	KindVerify = "verify"
)

// Run is a unit of work assigned to an agent.
type Run struct {
	ID   int64  `json:"id"`
	Kind string `json:"kind"`
	// JobID and Retention: after a backup the agent removes snapshots of
	// this job that the policy no longer keeps and prunes the repository.
	JobID      int64                 `json:"job_id,omitempty"`
	Retention  *repo.RetentionPolicy `json:"retention,omitempty"`
	JobName    string                `json:"job_name,omitempty"`
	Repository Repository            `json:"repository"`

	// Backup
	Paths    []string `json:"paths,omitempty"`
	Excludes []string `json:"excludes,omitempty"`

	// Restore
	SnapshotID    string   `json:"snapshot_id,omitempty"`
	RestoreTarget string   `json:"restore_target,omitempty"` // "" = original location
	Includes      []string `json:"includes,omitempty"`
	Verify        bool     `json:"verify,omitempty"`

	// Image backup: disk number and partitions (empty = whole disk).
	ImageDisk       int   `json:"image_disk,omitempty"`
	ImagePartitions []int `json:"image_partitions,omitempty"`

	// Image restore: disk to overwrite.
	TargetDisk  int  `json:"target_disk,omitempty"`
	KeepOffline bool `json:"keep_offline,omitempty"`

	// Image file restore: partition to read; Includes lists paths inside it
	// (`\Users\ana`) and RestoreTarget the folder ("" = original location).
	ImagePartition int `json:"image_partition,omitempty"`

	// Copy: Source is the repository to copy from; snapshots tagged
	// SourceTag are copied into Repository.
	Source    *Repository `json:"source,omitempty"`
	SourceTag string      `json:"source_tag,omitempty"`

	// Restore test: sample size.
	VerifyFiles    int    `json:"verify_files,omitempty"`
	VerifyMaxBytes uint64 `json:"verify_max_bytes,omitempty"`
	VerifyBlocks   int    `json:"verify_blocks,omitempty"`
}

// Repository tells the agent where and how to store data.
type Repository struct {
	URL          string `json:"url"`
	SFTPPassword string `json:"sftp_password,omitempty"`
	SFTPKey      string `json:"sftp_key,omitempty"` // PEM
	SFTPHostKey  string `json:"sftp_host_key,omitempty"`
	S3AccessKey  string `json:"s3_access_key,omitempty"`
	S3SecretKey  string `json:"s3_secret_key,omitempty"`
	S3Region     string `json:"s3_region,omitempty"`
	// S3LockDays makes every written object immutable for that many days.
	S3LockDays          int    `json:"s3_lock_days,omitempty"`
	SMBPassword         string `json:"smb_password,omitempty"`
	SMBDomain           string `json:"smb_domain,omitempty"`
	AzureKey            string `json:"azure_key,omitempty"`
	AzureSAS            string `json:"azure_sas,omitempty"`
	HardenedKey         string `json:"hardened_key,omitempty"`
	HardenedFingerprint string `json:"hardened_fingerprint,omitempty"`
	// Password unlocks (or, for a new repository, encrypts) the repository.
	Password string `json:"password,omitempty"`
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
	// Details carries kind specific data, e.g. the disk layout of an image.
	Details json.RawMessage `json:"details,omitempty"`
	// Forgotten lists snapshot IDs removed by the retention policy.
	Forgotten []string `json:"forgotten,omitempty"`
	// RepoURL is the actual repository location when the run was given a
	// pattern (rotating USB disks: the label of the disk used).
	RepoURL string `json:"repo_url,omitempty"`
}

// Error is returned by the server with non-2xx responses.
type Error struct {
	Error string `json:"error"`
}
