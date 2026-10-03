// Package api defines the JSON messages exchanged between agents and the
// management server.
//
// All agent endpoints live under /api/agent/ and, except enroll, require
// "Authorization: Bearer <agent-uuid>:<secret>".
package api

import (
	"encoding/json"
	"time"

	"github.com/backupzit/backupzit/internal/repo"
)

const (
	PathEnroll = "/api/agent/enroll"
	PathPoll   = "/api/agent/poll"
	// PathRunStart and PathRunFinish take the run id as a suffix: /api/agent/runs/{id}/start
	PathRunsPrefix = "/api/agent/runs/"
	// PathDownloadPrefix serves agent installers to agents (self-update).
	PathDownloadPrefix = "/api/agent/download/"
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
	// WantStatus asks for the job summary shown by the tray app.
	WantStatus bool `json:"want_status,omitempty"`
	// IPs are the agent's own network addresses (no loopback).
	IPs []string `json:"ips,omitempty"`
	// Disks is the disk inventory (JSON array of imaging.Disk). Agents send
	// it on start and periodically; nil means "unchanged / not included".
	Disks json.RawMessage `json:"disks,omitempty"`
	// Hypervisor is the Proxmox VE inventory (JSON pve.Inventory) of an agent
	// on a Proxmox node, sent like Disks.
	Hypervisor json.RawMessage `json:"hypervisor,omitempty"`
}

// PollResponse returns work for the agent.
type PollResponse struct {
	Run             *Run `json:"run,omitempty"`
	PollIntervalSec int  `json:"poll_interval_sec"`
	// Status is sent when the agent asks for it (WantStatus), for the tray
	// app on the machine.
	Status *AgentStatus `json:"status,omitempty"`
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
	// KindVMBackup backs up Proxmox VE virtual machines and containers from
	// the agent on the node, without agents in the guests.
	KindVMBackup = "vm-backup"
	// KindVMRestore recreates a guest from a vm-backup on a node.
	KindVMRestore = "vm-restore"
	// KindSystemBackup backs up a whole Linux system (files and disk layout);
	// KindSystemRestore recreates it on an empty disk or as a Proxmox VM.
	KindSystemBackup  = "system-backup"
	KindSystemRestore = "system-restore"
	// KindVMFileRestore copies files out of a VM disk in a Proxmox backup.
	KindVMFileRestore = "vm-file-restore"
	// KindAgentUpdate installs a newer agent version from the console.
	KindAgentUpdate = "agent-update"
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
	// NewHardware prepares the restored Windows for different hardware;
	// DriverPath lists extra driver folders (separated by ;).
	NewHardware bool   `json:"new_hardware,omitempty"`
	DriverPath  string `json:"driver_path,omitempty"`

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

	// VM backup: guest IDs ("*" = all guests on the node) and guests to skip.
	VMs       []string `json:"vms,omitempty"`
	VMExclude []string `json:"vm_exclude,omitempty"`
	// VMware: the ESXi host a vm-backup or vm-restore runs against (the
	// agent acts as proxy). Nil for Proxmox and Hyper-V, where the agent
	// runs on the hypervisor itself.
	VMware *VMwareHost `json:"vmware,omitempty"`
	// VM restore.
	VMRestore *VMRestore `json:"vm_restore,omitempty"`

	// VM file restore: files inside a guest disk.
	VMFiles *VMFileRestore `json:"vm_files,omitempty"`

	// System restore.
	SystemRestore *SystemRestore `json:"system_restore,omitempty"`

	// Agent update: installer file (downloaded from PathDownloadPrefix), its
	// SHA-256 and the version it installs.
	UpdateFile    string `json:"update_file,omitempty"`
	UpdateSHA256  string `json:"update_sha256,omitempty"`
	UpdateVersion string `json:"update_version,omitempty"`
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

// VMRestore are the options of a vm-restore run.
type VMRestore struct {
	VMID int `json:"vmid"` // guest in the backup
	// NewVMID: 0 = original ID, -1 = next free ID.
	NewVMID   int    `json:"new_vmid"`
	Name      string `json:"name,omitempty"`
	Storage   string `json:"storage,omitempty"` // "" = original storage
	Overwrite bool   `json:"overwrite,omitempty"`
	Start     bool   `json:"start,omitempty"`
}

// PathJobRunPrefix lets an agent start one of its own jobs ("Back up now"
// in the tray app): POST PathJobRunPrefix + "{id}/run".
const PathJobRunPrefix = "/api/agent/jobs/"

// AgentStatus summarises the agent's jobs for the tray app.
type AgentStatus struct {
	Jobs []JobStatus `json:"jobs"`
}

// JobStatus is one job in AgentStatus.
type JobStatus struct {
	ID           int64      `json:"id"`
	Name         string     `json:"name"`
	Kind         string     `json:"kind"`
	Enabled      bool       `json:"enabled"`
	Schedule     string     `json:"schedule"` // human readable
	NextRun      *time.Time `json:"next_run,omitempty"`
	LastStatus   string     `json:"last_status,omitempty"`
	LastFinished *time.Time `json:"last_finished,omitempty"`
	LastMessage  string     `json:"last_message,omitempty"`
	Running      bool       `json:"running,omitempty"`
}

// SystemRestore are the options of a system-restore run.
type SystemRestore struct {
	// Mode "disk": overwrite Device on the agent; "pve-vm": create a new
	// Proxmox VM on the agent's node and restore into it.
	Mode   string `json:"mode"`
	Device string `json:"device,omitempty"`
	// NewHardware adapts network settings and the initramfs.
	NewHardware bool `json:"new_hardware,omitempty"`
	// Proxmox VM.
	VMID    int    `json:"vmid,omitempty"` // 0 or -1 = next free ID
	Name    string `json:"name,omitempty"`
	Storage string `json:"storage,omitempty"`
	Memory  int    `json:"memory,omitempty"` // MiB
	Cores   int    `json:"cores,omitempty"`
	Bridge  string `json:"bridge,omitempty"`
	Start   bool   `json:"start,omitempty"`
}

// VMFileRestore selects files inside a VM disk of a Proxmox backup.
type VMFileRestore struct {
	VMID   int      `json:"vmid"`
	Disk   string   `json:"disk"`
	Volume string   `json:"volume"`
	Paths  []string `json:"paths"`
	Target string   `json:"target"` // folder on the agent
}

// VMwareHost is how a proxy agent reaches an ESXi host. The certificate
// and SSH host key are pinned.
type VMwareHost struct {
	Name       string `json:"name"`
	Address    string `json:"address"`
	User       string `json:"user"`
	Password   string `json:"password"`
	Thumbprint string `json:"thumbprint"`
	SSHHostKey string `json:"ssh_host_key,omitempty"`
}
