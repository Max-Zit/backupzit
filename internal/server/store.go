package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/backupzit/backupzit/internal/api"
	"github.com/backupzit/backupzit/internal/imaging"
	"github.com/backupzit/backupzit/internal/repo"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// Store wraps all database access.
type Store struct {
	db  *pgxpool.Pool
	box *secretBox // encrypts stored secrets; nil = plaintext (tests)
}

func NewStore(db *pgxpool.Pool) *Store { return &Store{db: db} }

var ErrNotFound = errors.New("not found")

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// randomToken returns a URL-safe random string with n bytes of entropy.
func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(t string) []byte {
	h := sha256.Sum256([]byte(t))
	return h[:]
}

// dummyHash is compared against when a username does not exist, so login
// takes the same time whether or not the user exists.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("backupzit-dummy"), bcrypt.DefaultCost)

func subtleEqual(a, b []byte) bool { return subtle.ConstantTimeCompare(a, b) == 1 }

// RandomPassword returns a random password for generated accounts.
func RandomPassword() string { return randomToken(15) }

// ---- users & sessions

func (s *Store) EnsureAdmin(ctx context.Context, username, password string) (created bool, err error) {
	var n int
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return false, err
	}
	_, err = s.db.Exec(ctx, `INSERT INTO users(username, password_hash, role) VALUES($1,$2,'admin')`, username, string(h))
	return err == nil, err
}

// SetPassword changes (or creates) a local administrator's password; used
// to recover access from the server's command line.
func (s *Store) SetPassword(ctx context.Context, username, password string) error {
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `INSERT INTO users(username, password_hash, role) VALUES($1,$2,'admin')
		ON CONFLICT (username) DO UPDATE SET password_hash=EXCLUDED.password_hash, role='admin', source='local', disabled=false`, username, string(h))
	if err != nil {
		return err
	}
	// A new password signs out every existing session of the user.
	_, err = s.db.Exec(ctx, `DELETE FROM sessions WHERE user_id=(SELECT id FROM users WHERE username=$1)`, username)
	return err
}

func (s *Store) Logout(ctx context.Context, token string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM sessions WHERE token_hash=$1`, hashToken(token))
	return err
}

// ---- storage targets

type Target struct {
	ID                  int64
	Name                string
	Kind                string
	URL                 string
	SFTPPassword        string
	SFTPKey             string
	SFTPHostKey         string
	S3AccessKey         string
	S3SecretKey         string
	S3Region            string
	S3LockDays          int // Object Lock retention for new objects, 0 = off
	SMBPassword         string
	SMBDomain           string
	HardenedKey         string
	HardenedFingerprint string
	AzureKey            string
	AzureSAS            string
	// Encrypted targets protect every repository with RecoveryKey.
	Encrypted   bool
	RecoveryKey string
	CreatedAt   time.Time
}

func (s *Store) CreateTarget(ctx context.Context, t Target) (int64, error) {
	if t.Encrypted && t.RecoveryKey == "" {
		t.RecoveryKey = NewRecoveryKey()
	}
	if !t.Encrypted {
		t.RecoveryKey = ""
	}
	t.Name = strings.TrimSpace(t.Name)
	t.URL = strings.TrimRight(strings.TrimSpace(t.URL), "/")
	if t.Name == "" || t.URL == "" {
		return 0, errors.New("name and location are required")
	}
	if t.Kind != "s3" {
		t.S3LockDays = 0
	}
	switch t.Kind {
	case "sftp":
		if !strings.HasPrefix(t.URL, "sftp://") {
			return 0, errors.New("SFTP location must look like sftp://user@host:22/path")
		}
		if t.SFTPHostKey == "" {
			return 0, errors.New("SFTP host key fingerprint is required (SHA256:...)")
		}
		if t.SFTPPassword == "" && t.SFTPKey == "" {
			return 0, errors.New("SFTP password or private key is required")
		}
	case "s3":
		if !strings.HasPrefix(t.URL, "s3://") {
			return 0, errors.New("S3 location must look like s3://endpoint/bucket/prefix")
		}
		if t.S3AccessKey == "" || t.S3SecretKey == "" {
			return 0, errors.New("S3 access key and secret key are required")
		}
	case "smb":
		if !strings.HasPrefix(t.URL, "smb://") {
			return 0, errors.New("SMB location must look like smb://user@host/share/path")
		}
		if t.SMBPassword == "" {
			return 0, errors.New("SMB password is required")
		}
	case "usb":
		if !strings.HasPrefix(t.URL, "usb://") {
			return 0, errors.New("removable disk location must look like usb://LABEL/folder")
		}
	case "azure":
		if !strings.HasPrefix(t.URL, "azure://") {
			return 0, errors.New("Azure location must look like azure://account/container/folder")
		}
		if t.AzureKey == "" && t.AzureSAS == "" {
			return 0, errors.New("the storage account key or a SAS token is required")
		}
	case "hardened":
		if !strings.HasPrefix(t.URL, "hardened://") {
			return 0, errors.New("hardened repository location must look like hardened://host:8500/path")
		}
		if t.HardenedKey == "" || !strings.HasPrefix(t.HardenedFingerprint, "SHA256:") {
			return 0, errors.New("access key and certificate fingerprint (SHA256:...) of the hardened repository are required")
		}
	case "local":
	default:
		return 0, fmt.Errorf("unknown target type %q", t.Kind)
	}
	s.encryptTarget(&t)
	var id int64
	err := s.db.QueryRow(ctx, `INSERT INTO storage_targets(name,kind,url,sftp_password,sftp_key,sftp_host_key,s3_access_key,s3_secret_key,s3_region,smb_password,smb_domain,encrypted,repo_password,s3_lock_days,hardened_key,hardened_fingerprint,azure_key,azure_sas)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18) RETURNING id`,
		t.Name, t.Kind, t.URL, t.SFTPPassword, t.SFTPKey, strings.TrimSpace(t.SFTPHostKey), strings.TrimSpace(t.S3AccessKey), t.S3SecretKey, strings.TrimSpace(t.S3Region), t.SMBPassword, strings.TrimSpace(t.SMBDomain), t.Encrypted, t.RecoveryKey, t.S3LockDays, t.HardenedKey, t.HardenedFingerprint, t.AzureKey, t.AzureSAS).Scan(&id)
	if err != nil && strings.Contains(err.Error(), "duplicate key") {
		return 0, errors.New("a storage target with that name already exists")
	}
	return id, err
}

const targetCols = `id, name, kind, url, sftp_password, sftp_key, sftp_host_key, created_at, s3_access_key, s3_secret_key, s3_region, smb_password, smb_domain, encrypted, repo_password, s3_lock_days, hardened_key, hardened_fingerprint, azure_key, azure_sas`

func scanTarget(r pgx.Row) (Target, error) {
	var t Target
	err := r.Scan(&t.ID, &t.Name, &t.Kind, &t.URL, &t.SFTPPassword, &t.SFTPKey, &t.SFTPHostKey, &t.CreatedAt, &t.S3AccessKey, &t.S3SecretKey, &t.S3Region, &t.SMBPassword, &t.SMBDomain, &t.Encrypted, &t.RecoveryKey, &t.S3LockDays, &t.HardenedKey, &t.HardenedFingerprint, &t.AzureKey, &t.AzureSAS)
	return t, err
}

func (s *Store) ListTargets(ctx context.Context) ([]Target, error) {
	rows, err := s.db.Query(ctx, `SELECT `+targetCols+` FROM storage_targets ORDER BY name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Target, error) {
		t, err := scanTarget(r)
		if err == nil {
			err = s.decryptTarget(&t)
		}
		return t, err
	})
}

func (s *Store) GetTarget(ctx context.Context, id int64) (Target, error) {
	t, err := scanTarget(s.db.QueryRow(ctx, `SELECT `+targetCols+` FROM storage_targets WHERE id=$1`, id))
	if err == nil {
		err = s.decryptTarget(&t)
	}
	return t, notFound(err)
}

func (s *Store) DeleteTarget(ctx context.Context, id int64) error {
	_, err := s.db.Exec(ctx, `DELETE FROM storage_targets WHERE id=$1`, id)
	if err != nil && strings.Contains(err.Error(), "violates foreign key") {
		return errors.New("target is used by jobs; delete them first")
	}
	return err
}

// ---- enrollment & agents

// CreateEnrollmentToken returns a new token valid for ttl.
func (s *Store) CreateEnrollmentToken(ctx context.Context, ttl time.Duration) (string, time.Time, error) {
	tok := randomToken(24)
	exp := time.Now().Add(ttl)
	_, err := s.db.Exec(ctx, `INSERT INTO enrollment_tokens(token_hash, expires_at) VALUES($1,$2)`, hashToken(tok), exp)
	return tok, exp, err
}

var unsafeDirChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// agentRepoDir builds a readable, unique repository directory name.
func agentRepoDir(hostname, uuid string) string {
	h := strings.Trim(unsafeDirChars.ReplaceAllString(strings.ToLower(hostname), "-"), "-.")
	if h == "" {
		h = "agent"
	}
	if len(h) > 40 {
		h = h[:40]
	}
	return h + "_" + strings.ReplaceAll(uuid, "-", "")[:8]
}

// EnrollAgent validates a token and registers a new agent.
func (s *Store) EnrollAgent(ctx context.Context, req api.EnrollRequest) (api.EnrollResponse, error) {
	var ok bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM enrollment_tokens WHERE token_hash=$1 AND expires_at > now())`,
		hashToken(req.Token)).Scan(&ok)
	if err != nil {
		return api.EnrollResponse{}, err
	}
	if !ok {
		return api.EnrollResponse{}, errors.New("invalid or expired enrollment token")
	}
	if strings.TrimSpace(req.Hostname) == "" {
		return api.EnrollResponse{}, errors.New("hostname is required")
	}
	req.Hostname, req.OS, req.Arch, req.Version = clip(req.Hostname, 255), clip(req.OS, 255), clip(req.Arch, 32), clip(req.Version, 64)
	resp := api.EnrollResponse{AgentUUID: newUUID(), Secret: randomToken(32)}
	_, err = s.db.Exec(ctx, `INSERT INTO agents(uuid, secret_hash, hostname, repo_dir, os, arch, version, last_seen_at, recovery)
		VALUES($1,$2,$3,$4,$5,$6,$7,now(),$8)`,
		resp.AgentUUID, hashToken(resp.Secret), req.Hostname, agentRepoDir(req.Hostname, resp.AgentUUID), req.OS, req.Arch, req.Version, req.Recovery)
	return resp, err
}

// clip bounds strings reported by agents.
func clip(s string, n int) string {
	if len(s) > n {
		return strings.ToValidUTF8(s[:n], "")
	}
	return s
}

func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

type Agent struct {
	ID         int64
	UUID       string
	Hostname   string
	RepoDir    string
	OS         string
	Arch       string
	Version    string
	EnrolledAt time.Time
	LastSeen   *time.Time
	// Inventory is the agent's disk list (JSON array of imaging.Disk).
	Inventory   json.RawMessage
	InventoryAt *time.Time
	// Recovery agents run from boot media to restore a machine.
	Recovery bool
	// LocalIPs are reported by the agent; RemoteAddr is the address its
	// requests come from (the public address when it is behind NAT).
	LocalIPs   []string
	RemoteAddr string
	// Hypervisor is the Proxmox VE inventory of an agent on a Proxmox node.
	Hypervisor json.RawMessage
}

// Disks decodes the reported disk inventory.
func (a Agent) Disks() []imaging.Disk {
	var d []imaging.Disk
	if len(a.Inventory) > 0 {
		json.Unmarshal(a.Inventory, &d)
	}
	return d
}

// Online reports whether the agent polled recently.
func (a Agent) Online() bool {
	return a.LastSeen != nil && time.Since(*a.LastSeen) < 3*time.Minute
}

const agentCols = `id, uuid, hostname, repo_dir, os, arch, version, enrolled_at, last_seen_at, inventory, inventory_at, recovery, local_ips, remote_addr, hypervisor`

func scanAgent(r pgx.Row, extra ...any) (Agent, error) {
	var a Agent
	dest := append([]any{&a.ID, &a.UUID, &a.Hostname, &a.RepoDir, &a.OS, &a.Arch, &a.Version, &a.EnrolledAt, &a.LastSeen, &a.Inventory, &a.InventoryAt, &a.Recovery, &a.LocalIPs, &a.RemoteAddr, &a.Hypervisor}, extra...)
	err := r.Scan(dest...)
	return a, err
}

// AuthenticateAgent checks "uuid:secret" credentials.
func (s *Store) AuthenticateAgent(ctx context.Context, uuid, secret string) (Agent, error) {
	var hash []byte
	a, err := scanAgent(s.db.QueryRow(ctx, `SELECT `+agentCols+`, secret_hash FROM agents WHERE uuid=$1`, uuid), &hash)
	if err != nil {
		return Agent{}, errors.New("unknown agent")
	}
	if subtleEqual(hash, hashToken(secret)) {
		return a, nil
	}
	return Agent{}, errors.New("invalid agent credentials")
}

func (s *Store) TouchAgent(ctx context.Context, id int64, req api.PollRequest, remote string) error {
	req.Hostname, req.OS, req.Arch, req.Version = clip(req.Hostname, 255), clip(req.OS, 255), clip(req.Arch, 32), clip(req.Version, 64)
	if len(req.Disks) > 0 {
		if _, err := s.db.Exec(ctx, `UPDATE agents SET inventory=$2, inventory_at=now() WHERE id=$1`, id, req.Disks); err != nil {
			return err
		}
	}
	if len(req.Hypervisor) > 0 && len(req.Hypervisor) < 4<<20 {
		if _, err := s.db.Exec(ctx, `UPDATE agents SET hypervisor=$2, hypervisor_at=now() WHERE id=$1`, id, req.Hypervisor); err != nil {
			return err
		}
	}
	if len(req.IPs) > 16 {
		req.IPs = req.IPs[:16]
	}
	for i := range req.IPs {
		req.IPs[i] = clip(req.IPs[i], 64)
	}
	if req.IPs == nil {
		req.IPs = []string{}
	}
	if _, err := s.db.Exec(ctx, `UPDATE agents SET local_ips=$2, remote_addr=$3 WHERE id=$1`, id, req.IPs, clip(remote, 64)); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `UPDATE agents SET last_seen_at=now(),
		hostname=COALESCE(NULLIF($2,''),hostname), os=COALESCE(NULLIF($3,''),os),
		arch=COALESCE(NULLIF($4,''),arch), version=COALESCE(NULLIF($5,''),version) WHERE id=$1`,
		id, req.Hostname, req.OS, req.Arch, req.Version)
	return err
}

func (s *Store) ListAgents(ctx context.Context) ([]Agent, error) {
	rows, err := s.db.Query(ctx, `SELECT `+agentCols+` FROM agents ORDER BY hostname`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Agent, error) { return scanAgent(r) })
}

func (s *Store) GetAgent(ctx context.Context, id int64) (Agent, error) {
	a, err := scanAgent(s.db.QueryRow(ctx, `SELECT `+agentCols+` FROM agents WHERE id=$1`, id))
	return a, notFound(err)
}

func (s *Store) DeleteAgent(ctx context.Context, id int64) error {
	_, err := s.db.Exec(ctx, `DELETE FROM agents WHERE id=$1`, id)
	return err
}

// ---- jobs

type Job struct {
	ID int64
	// Kind is JobFiles or JobImage.
	Kind string
	// Retention decides which of the job's backups are kept.
	Retention repo.RetentionPolicy
	// Image jobs: disk number and partitions (nil = whole disk).
	ImageDisk       *int
	ImagePartitions []int
	AgentID         int64
	Hostname        string
	TargetID        int64
	TargetName      string
	Name            string
	Paths           []string
	Excludes        []string
	Schedule        string
	Enabled         bool
	LastSched       *time.Time
	// Copy jobs: the job whose backups are copied.
	SourceJobID   *int64
	SourceJobName *string
	// RetentionHold pauses retention after a suspicious backup.
	RetentionHold bool
	// VMware VM jobs: the ESXi host (AgentID is its proxy agent).
	VMwareHostID   *int64
	VMwareHostName *string
	// Options: speed limit, commands before and after the backup.
	Options   JobOptions
	CreatedAt time.Time
	// Last run summary
	LastStatus   *string
	LastFinished *time.Time
}

// checkJob validates and normalizes a job before it is stored.
func (s *Store) checkJob(ctx context.Context, j *Job) error {
	j.Name = strings.TrimSpace(j.Name)
	if j.Kind == "" {
		j.Kind = JobFiles
	}
	if j.Name == "" {
		return errors.New("name is required")
	}
	switch j.Kind {
	case JobFiles:
		if len(j.Paths) == 0 {
			return errors.New("at least one folder or file is required")
		}
		j.ImageDisk, j.ImagePartitions = nil, nil
	case JobImage:
		if j.ImageDisk == nil {
			return errors.New("choose a disk to image")
		}
		j.Paths = []string{}
	case JobVM:
		j.ImageDisk, j.ImagePartitions = nil, nil
	case JobSystem:
		j.Paths, j.ImageDisk, j.ImagePartitions = []string{"/"}, nil, nil
	case JobSQL:
		// Paths are database names; none means all user databases.
		if j.Paths == nil {
			j.Paths = []string{}
		}
		for _, p := range j.Paths {
			if len(p) > 128 || strings.ContainsAny(p, "\x00") {
				return fmt.Errorf("invalid database name %q", p)
			}
		}
		j.ImageDisk, j.ImagePartitions = nil, nil
	case JobCopy:
		if j.SourceJobID == nil {
			return errors.New("choose the job whose backups are copied")
		}
		src, err := s.GetJob(ctx, *j.SourceJobID)
		if err != nil {
			return errors.New("unknown source job")
		}
		if src.Kind == JobCopy {
			return errors.New("choose a backup job, not another copy job")
		}
		if src.TargetID == j.TargetID {
			return errors.New("the copy must go to a different storage target than the source job")
		}
		j.AgentID = src.AgentID
		j.Paths, j.ImageDisk, j.ImagePartitions = []string{}, nil, nil
	default:
		return fmt.Errorf("unknown job kind %q", j.Kind)
	}
	sc, err := ParseSchedule(j.Schedule)
	if err != nil {
		return err
	}
	if sc.Kind == SchedAfter && j.Kind != JobCopy {
		return errors.New("\"after each backup\" is only available for copy jobs")
	}
	j.Schedule = sc.Encode()
	if j.Kind == JobVM && j.VMwareHostID != nil {
		if h, err := s.GetVMwareHost(ctx, *j.VMwareHostID); err == nil && h.ProxyAgentID != nil {
			j.AgentID = *h.ProxyAgentID
		}
	}
	agent, err := s.GetAgent(ctx, j.AgentID)
	if err != nil {
		return errors.New("unknown agent")
	}
	if j.Kind == JobSQL && (!strings.Contains(strings.ToLower(agent.OS), "windows") || agent.Recovery) {
		return fmt.Errorf("%s is not a Windows machine; SQL Server jobs run on the Windows machine with SQL Server", agent.Hostname)
	}
	if j.Kind != JobSQL {
		j.Options.SQLInstance, j.Options.SQLSystem, j.Options.SQLLogMinutes = "", false, 0
	}
	if j.Kind == JobSystem && (strings.Contains(strings.ToLower(agent.OS), "windows") || agent.Recovery) {
		return fmt.Errorf("%s is not a Linux machine; use a disk image job for Windows", agent.Hostname)
	}
	if j.Kind == JobVM && j.VMwareHostID != nil {
		h, err := s.GetVMwareHost(ctx, *j.VMwareHostID)
		if err != nil {
			return errors.New("unknown VMware host")
		}
		if h.ProxyAgentID == nil {
			return fmt.Errorf("choose a proxy agent for %s first", h.Name)
		}
		j.AgentID = *h.ProxyAgentID
		if err := checkVMwareSelection(h, j.Paths, j.Excludes); err != nil {
			return err
		}
	} else if j.Kind == JobVM {
		if err := checkVMSelection(agent, j.Paths, j.Excludes); err != nil {
			return err
		}
	} else {
		j.VMwareHostID = nil
	}
	if j.Kind == JobImage {
		if err := checkImageSelection(agent, *j.ImageDisk, j.ImagePartitions); err != nil {
			return err
		}
	}
	if _, err := s.GetTarget(ctx, j.TargetID); err != nil {
		return errors.New("unknown storage target")
	}
	if j.Excludes == nil {
		j.Excludes = []string{}
	}
	if err := j.Options.Validate(); err != nil {
		return err
	}
	return s.checkReplica(ctx, j, agent)
}

func (s *Store) CreateJob(ctx context.Context, j Job) (int64, error) {
	if err := s.checkJob(ctx, &j); err != nil {
		return 0, err
	}
	var id int64
	err := s.db.QueryRow(ctx, `INSERT INTO jobs(agent_id, target_id, name, paths, excludes, schedule, enabled, last_scheduled_at, kind, image_disk, image_partitions, retention, source_job_id, vmware_host_id, options)
		VALUES($1,$2,$3,$4,$5,$6,$7,now(),$8,$9,$10,$11,$12,$13,$14) RETURNING id`,
		j.AgentID, j.TargetID, j.Name, j.Paths, j.Excludes, j.Schedule, j.Enabled, j.Kind, j.ImageDisk, j.ImagePartitions, j.Retention, j.SourceJobID, j.VMwareHostID, j.Options).Scan(&id)
	return id, err
}

const jobCols = `j.id, j.kind, j.image_disk, j.image_partitions, j.retention, j.agent_id, a.hostname, j.target_id, st.name, j.name, j.paths, j.excludes,
	j.schedule, j.enabled, j.last_scheduled_at, j.created_at,
	(SELECT r.status FROM runs r WHERE r.job_id=j.id AND r.kind IN ('backup','image-backup','vm-backup','system-backup','copy','sql-backup','sql-log') ORDER BY r.queued_at DESC LIMIT 1),
	(SELECT r.finished_at FROM runs r WHERE r.job_id=j.id AND r.kind IN ('backup','image-backup','vm-backup','system-backup','copy','sql-backup','sql-log') ORDER BY r.queued_at DESC LIMIT 1),
	j.source_job_id, (SELECT sj.name FROM jobs sj WHERE sj.id=j.source_job_id), j.retention_hold,
	j.vmware_host_id, (SELECT vh.name FROM vmware_hosts vh WHERE vh.id=j.vmware_host_id), j.options`

const jobFrom = ` FROM jobs j JOIN agents a ON a.id=j.agent_id JOIN storage_targets st ON st.id=j.target_id`

func scanJob(r pgx.Row) (Job, error) {
	var j Job
	err := r.Scan(&j.ID, &j.Kind, &j.ImageDisk, &j.ImagePartitions, &j.Retention, &j.AgentID, &j.Hostname, &j.TargetID, &j.TargetName, &j.Name,
		&j.Paths, &j.Excludes, &j.Schedule, &j.Enabled, &j.LastSched, &j.CreatedAt, &j.LastStatus, &j.LastFinished, &j.SourceJobID, &j.SourceJobName, &j.RetentionHold, &j.VMwareHostID, &j.VMwareHostName, &j.Options)
	return j, err
}

func (s *Store) ListJobs(ctx context.Context) ([]Job, error) {
	rows, err := s.db.Query(ctx, `SELECT `+jobCols+jobFrom+` ORDER BY a.hostname, j.name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Job, error) { return scanJob(r) })
}

func (s *Store) GetJob(ctx context.Context, id int64) (Job, error) {
	j, err := scanJob(s.db.QueryRow(ctx, `SELECT `+jobCols+jobFrom+` WHERE j.id=$1`, id))
	return j, notFound(err)
}

func (s *Store) SetJobEnabled(ctx context.Context, id int64, enabled bool) error {
	_, err := s.db.Exec(ctx, `UPDATE jobs SET enabled=$2, last_scheduled_at=now() WHERE id=$1`, id, enabled)
	return err
}

func (s *Store) DeleteJob(ctx context.Context, id int64) error {
	_, err := s.db.Exec(ctx, `DELETE FROM jobs WHERE id=$1`, id)
	return err
}

// repoURL is the repository location of an agent on a target: every agent
// gets its own repository below the target's base location.
func repoURL(t Target, a Agent) string {
	if t.Kind == "local" {
		return strings.TrimRight(t.URL, `/\`) + "/" + a.RepoDir
	}
	// Insert the agent directory into the path, keeping any query (?tls=false).
	if u, err := url.Parse(t.URL); err == nil {
		u.Path = path.Join(u.Path, a.RepoDir)
		return u.String()
	}
	return t.URL + "/" + path.Clean(a.RepoDir)
}

// ErrRunActive is returned when a job already has a queued or running run.
var ErrRunActive = errors.New("job already has a queued or running backup")

// QueueBackup creates a queued backup run for a job.
func (s *Store) QueueBackup(ctx context.Context, jobID int64, trigger string) (int64, error) {
	j, err := s.GetJob(ctx, jobID)
	if err != nil {
		return 0, err
	}
	a, err := s.GetAgent(ctx, j.AgentID)
	if err != nil {
		return 0, err
	}
	t, err := s.GetTarget(ctx, j.TargetID)
	if err != nil {
		return 0, err
	}
	kind := api.KindBackup
	if j.Kind == JobImage {
		kind = api.KindImageBackup
	}
	if j.Kind == JobVM {
		kind = api.KindVMBackup
	}
	if j.Kind == JobSystem {
		kind = api.KindSystemBackup
	}
	if j.Kind == JobSQL {
		kind = api.KindSQLBackup
	}
	if j.Kind == JobCopy {
		return s.queueCopy(ctx, j, a, t, trigger)
	}
	repo := repoURL(t, a)
	if j.VMwareHostID != nil {
		// A VMware host has its own repository, independent of its proxy.
		h, err := s.GetVMwareHost(ctx, *j.VMwareHostID)
		if err != nil {
			return 0, err
		}
		repo = repoURL(t, Agent{RepoDir: h.RepoDir})
	}
	var id int64
	err = s.db.QueryRow(ctx, `INSERT INTO runs(agent_id, job_id, kind, trigger, repo_url, target_id, paths, excludes, image_disk, image_partitions, vmware_host_id)
		SELECT $1,$2,$8,$3,$4,$5,$6,$7,$9,$10,$11
		WHERE NOT EXISTS (SELECT 1 FROM runs WHERE job_id=$2 AND kind IN ('backup','image-backup','vm-backup','system-backup','sql-backup','sql-log') AND status IN ('queued','running'))
		RETURNING id`,
		j.AgentID, j.ID, trigger, repo, t.ID, j.Paths, j.Excludes, kind, j.ImageDisk, j.ImagePartitions, j.VMwareHostID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrRunActive
	}
	return id, err
}

// QueueRestore creates a restore run from a successful backup run.
// agentID selects the agent that performs the restore; it may differ from
// the machine that was backed up.
func (s *Store) QueueRestore(ctx context.Context, backupRunID, agentID int64, target string, includes []string, verify bool) (int64, error) {
	b, err := s.GetRun(ctx, backupRunID)
	if err != nil {
		return 0, err
	}
	if (b.Kind != api.KindBackup && b.Kind != api.KindCopy && b.Kind != api.KindSystemBackup) || b.SnapshotID == "" {
		return 0, errors.New("run has no snapshot to restore")
	}
	if b.Kind == api.KindCopy && !s.copyOfFiles(ctx, b) {
		return 0, errors.New("copies of disk images are restored with the command line agent (see Documentation)")
	}
	if _, err := s.GetAgent(ctx, agentID); err != nil {
		return 0, errors.New("unknown agent")
	}
	if includes == nil {
		includes = []string{}
	}
	var id int64
	err = s.db.QueryRow(ctx, `INSERT INTO runs(agent_id, job_id, kind, trigger, repo_url, target_id, paths, snapshot_id, restore_target, restore_verify)
		VALUES($1,$2,'restore','manual',$3,$4,$5,$6,$7,$8) RETURNING id`,
		agentID, b.JobID, b.RepoURL, b.TargetID, includes, b.SnapshotID, target, verify).Scan(&id)
	return id, err
}

type Run struct {
	ID              int64
	AgentID         int64
	Hostname        string
	JobID           *int64
	JobName         *string
	Kind            string
	Status          string
	Trigger         string
	RepoURL         string
	TargetID        *int64
	Paths           []string
	Excludes        []string
	SnapshotID      string
	RestoreTarget   string
	RestoreVerify   bool
	ImageDisk       *int
	ImagePartitions []int
	TargetDisk      *int
	KeepOffline     bool
	Details         json.RawMessage
	Expired         bool
	// Copy runs: the repository copied from.
	SourceTargetID *int64
	SourceRepoURL  string
	// Anomaly explains why a backup looks like ransomware or mass deletion.
	Anomaly    string
	AnomalyAck bool
	// VMRestore holds the options of a vm-restore run (JSON api.VMRestore).
	VMRestore json.RawMessage
	// Image restore onto different hardware.
	NewHardware bool
	DriverPath  string
	// SystemRestore holds the options of a system-restore run (JSON).
	SystemRestore json.RawMessage
	QueuedAt      time.Time
	StartedAt     *time.Time
	FinishedAt    *time.Time
	Stats         json.RawMessage
	Errors        []string
	Message       string
}

// Duration returns how long the run took (or has been running).
func (r Run) Duration() time.Duration {
	if r.StartedAt == nil {
		return 0
	}
	end := time.Now()
	if r.FinishedAt != nil {
		end = *r.FinishedAt
	}
	return end.Sub(*r.StartedAt).Round(time.Second)
}

const runCols = `r.id, r.agent_id, a.hostname, r.job_id, j.name, r.kind, r.status, r.trigger, r.repo_url,
	r.target_id, r.paths, r.excludes, r.snapshot_id, r.restore_target, r.restore_verify, r.queued_at, r.started_at, r.finished_at,
	r.stats, r.errors, r.message, r.image_disk, r.image_partitions, r.target_disk, r.keep_offline, r.details, r.expired, r.source_target_id, r.source_repo_url, r.anomaly, r.anomaly_ack, r.vm_restore, r.new_hardware, r.driver_path, r.system_restore`

const runFrom = ` FROM runs r JOIN agents a ON a.id=r.agent_id LEFT JOIN jobs j ON j.id=r.job_id`

func scanRun(row pgx.Row) (Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.AgentID, &r.Hostname, &r.JobID, &r.JobName, &r.Kind, &r.Status,
		&r.Trigger, &r.RepoURL, &r.TargetID, &r.Paths, &r.Excludes, &r.SnapshotID, &r.RestoreTarget, &r.RestoreVerify,
		&r.QueuedAt, &r.StartedAt, &r.FinishedAt, &r.Stats, &r.Errors, &r.Message,
		&r.ImageDisk, &r.ImagePartitions, &r.TargetDisk, &r.KeepOffline, &r.Details, &r.Expired, &r.SourceTargetID, &r.SourceRepoURL, &r.Anomaly, &r.AnomalyAck, &r.VMRestore, &r.NewHardware, &r.DriverPath, &r.SystemRestore)
	return r, err
}

func (s *Store) GetRun(ctx context.Context, id int64) (Run, error) {
	r, err := scanRun(s.db.QueryRow(ctx, `SELECT `+runCols+runFrom+` WHERE r.id=$1`, id))
	return r, notFound(err)
}

// RunFilter narrows ListRuns.
type RunFilter struct {
	JobID   int64
	AgentID int64
	Limit   int
}

func (s *Store) ListRuns(ctx context.Context, f RunFilter) ([]Run, error) {
	if f.Limit <= 0 {
		f.Limit = 100
	}
	q := `SELECT ` + runCols + runFrom + ` WHERE ($1=0 OR r.job_id=$1) AND ($2=0 OR r.agent_id=$2) ORDER BY r.queued_at DESC LIMIT $3`
	rows, err := s.db.Query(ctx, q, f.JobID, f.AgentID, f.Limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Run, error) { return scanRun(r) })
}

// ClaimRun atomically assigns the oldest queued run of an agent.
func (s *Store) ClaimRun(ctx context.Context, agentID int64) (*Run, error) {
	var id int64
	err := s.db.QueryRow(ctx, `UPDATE runs SET status='running', started_at=now()
		WHERE id = (SELECT id FROM runs WHERE agent_id=$1 AND status='queued' ORDER BY kind='verify', queued_at LIMIT 1 FOR UPDATE SKIP LOCKED)
		RETURNING id`, agentID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r, err := s.GetRun(ctx, id)
	return &r, err
}

// FinishRun records the agent's result. Only runs of that agent in state
// running can be finished.
func (s *Store) FinishRun(ctx context.Context, agentID, runID int64, res api.RunResult) error {
	switch res.Status {
	case api.StatusSuccess, api.StatusWarning, api.StatusFailed:
	default:
		return fmt.Errorf("invalid status %q", res.Status)
	}
	if res.Errors == nil {
		res.Errors = []string{}
	}
	var stats, details any
	if len(res.Stats) > 0 {
		stats = res.Stats
	}
	if len(res.Details) > 0 {
		details = res.Details
	}
	ct, err := s.db.Exec(ctx, `UPDATE runs SET status=$3, finished_at=now(), stats=$4, errors=$5, message=$6,
		snapshot_id=CASE WHEN kind IN ('backup','image-backup','vm-backup','system-backup','copy','sql-backup','sql-log') THEN $7 ELSE snapshot_id END, details=$8
		WHERE id=$2 AND agent_id=$1 AND status='running'`,
		agentID, runID, res.Status, stats, res.Errors, res.Message, res.SnapshotID, details)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	if res.Status == api.StatusSuccess {
		if err := s.endInstant(ctx, runID); err != nil {
			return err
		}
	}
	if res.RepoURL != "" {
		s.recordUSBDisk(ctx, runID, res.RepoURL)
	}
	if len(res.Forgotten) > 0 {
		if _, err := s.db.Exec(ctx, `UPDATE runs SET expired=true WHERE snapshot_id = ANY($1)`, res.Forgotten); err != nil {
			return err
		}
	}
	if a, err := s.checkAnomaly(ctx, runID); err == nil && a != "" {
		slog.Warn("suspicious backup: possible ransomware or mass deletion", "run", runID, "why", a)
	}
	return nil
}

// FailStaleRuns marks running runs as failed when their agent stopped polling.
func (s *Store) FailStaleRuns(ctx context.Context, after time.Duration) (int64, error) {
	ct, err := s.db.Exec(ctx, `UPDATE runs r SET status='failed', finished_at=now(),
		message='agent stopped responding while the run was in progress'
		FROM agents a WHERE a.id=r.agent_id AND r.status='running'
		AND (a.last_seen_at IS NULL OR a.last_seen_at < now() - make_interval(secs => $1))`, after.Seconds())
	if err != nil {
		return 0, err
	}
	return ct.RowsAffected(), nil
}

// Summary holds dashboard counters.
type Summary struct {
	Agents, AgentsOnline, Jobs, Targets     int
	Runs24h, Failed24h, Warning24h, Running int
	Suspicious                              int
}

func (s *Store) Summary(ctx context.Context) (Summary, error) {
	var x Summary
	err := s.db.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM agents),
		(SELECT count(*) FROM agents WHERE last_seen_at > now() - interval '3 minutes'),
		(SELECT count(*) FROM jobs),
		(SELECT count(*) FROM storage_targets),
		(SELECT count(*) FROM runs WHERE queued_at > now() - interval '24 hours'),
		(SELECT count(*) FROM runs WHERE queued_at > now() - interval '24 hours' AND status='failed'),
		(SELECT count(*) FROM runs WHERE queued_at > now() - interval '24 hours' AND status='warning'),
		(SELECT count(*) FROM runs WHERE status IN ('queued','running')),
		(SELECT count(*) FROM runs WHERE anomaly<>'' AND NOT anomaly_ack)`).
		Scan(&x.Agents, &x.AgentsOnline, &x.Jobs, &x.Targets, &x.Runs24h, &x.Failed24h, &x.Warning24h, &x.Running, &x.Suspicious)
	return x, err
}

// NewRecoveryKey returns a random 160-bit key formatted for writing down,
// e.g. "K7QM-2XRD-...". It unlocks encrypted repositories.
func NewRecoveryKey() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	s := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b) // 32 chars
	var parts []string
	for i := 0; i < len(s); i += 4 {
		parts = append(parts, s[i:i+4])
	}
	return strings.Join(parts, "-")
}
