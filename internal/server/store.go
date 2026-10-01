package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/backupzit/backupzit/internal/api"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// Store wraps all database access.
type Store struct {
	db *pgxpool.Pool
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
	_, err = s.db.Exec(ctx, `INSERT INTO users(username, password_hash) VALUES($1,$2)`, username, string(h))
	return err == nil, err
}

// SetPassword changes (or creates) a user's password.
func (s *Store) SetPassword(ctx context.Context, username, password string) error {
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `INSERT INTO users(username, password_hash) VALUES($1,$2)
		ON CONFLICT (username) DO UPDATE SET password_hash=EXCLUDED.password_hash`, username, string(h))
	return err
}

// Login checks credentials and returns a new session token.
func (s *Store) Login(ctx context.Context, username, password string, ttl time.Duration) (string, error) {
	var id int64
	var hash string
	err := s.db.QueryRow(ctx, `SELECT id, password_hash FROM users WHERE username=$1`, username).Scan(&id, &hash)
	if err != nil {
		// Spend comparable time to avoid user enumeration by timing.
		bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return "", errors.New("invalid username or password")
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return "", errors.New("invalid username or password")
	}
	tok := randomToken(32)
	_, err = s.db.Exec(ctx, `INSERT INTO sessions(token_hash, user_id, expires_at) VALUES($1,$2,$3)`,
		hashToken(tok), id, time.Now().Add(ttl))
	return tok, err
}

// SessionUser returns the username of a valid session.
func (s *Store) SessionUser(ctx context.Context, token string) (string, error) {
	var u string
	err := s.db.QueryRow(ctx, `SELECT u.username FROM sessions s JOIN users u ON u.id=s.user_id
		WHERE s.token_hash=$1 AND s.expires_at > now()`, hashToken(token)).Scan(&u)
	return u, notFound(err)
}

func (s *Store) Logout(ctx context.Context, token string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM sessions WHERE token_hash=$1`, hashToken(token))
	return err
}

// ---- tenants

type Tenant struct {
	ID        int64
	Name      string
	Slug      string
	CreatedAt time.Time
	Agents    int
	Jobs      int
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(name string) string {
	s := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if s == "" {
		s = "tenant"
	}
	return s
}

func (s *Store) CreateTenant(ctx context.Context, name string) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, errors.New("tenant name is required")
	}
	var id int64
	err := s.db.QueryRow(ctx, `INSERT INTO tenants(name, slug) VALUES($1,$2) RETURNING id`, name, slugify(name)).Scan(&id)
	if err != nil && strings.Contains(err.Error(), "duplicate key") {
		return 0, errors.New("a tenant with this name already exists")
	}
	return id, err
}

func (s *Store) ListTenants(ctx context.Context) ([]Tenant, error) {
	rows, err := s.db.Query(ctx, `SELECT t.id, t.name, t.slug, t.created_at,
		(SELECT count(*) FROM agents a WHERE a.tenant_id=t.id),
		(SELECT count(*) FROM jobs j WHERE j.tenant_id=t.id)
		FROM tenants t ORDER BY t.name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Tenant, error) {
		var t Tenant
		err := r.Scan(&t.ID, &t.Name, &t.Slug, &t.CreatedAt, &t.Agents, &t.Jobs)
		return t, err
	})
}

func (s *Store) DeleteTenant(ctx context.Context, id int64) error {
	_, err := s.db.Exec(ctx, `DELETE FROM tenants WHERE id=$1`, id)
	if err != nil && strings.Contains(err.Error(), "violates foreign key") {
		return errors.New("delete the tenant's jobs first")
	}
	return err
}

// ---- storage targets

type Target struct {
	ID           int64
	TenantID     int64
	TenantName   string
	Name         string
	Kind         string
	URL          string
	SFTPPassword string
	SFTPKey      string
	SFTPHostKey  string
	CreatedAt    time.Time
}

func (s *Store) CreateTarget(ctx context.Context, t Target) (int64, error) {
	t.Name = strings.TrimSpace(t.Name)
	t.URL = strings.TrimRight(strings.TrimSpace(t.URL), "/")
	if t.Name == "" || t.URL == "" {
		return 0, errors.New("name and location are required")
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
	case "local":
	default:
		return 0, fmt.Errorf("unknown target type %q", t.Kind)
	}
	var id int64
	err := s.db.QueryRow(ctx, `INSERT INTO storage_targets(tenant_id,name,kind,url,sftp_password,sftp_key,sftp_host_key)
		VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		t.TenantID, t.Name, t.Kind, t.URL, t.SFTPPassword, t.SFTPKey, strings.TrimSpace(t.SFTPHostKey)).Scan(&id)
	if err != nil && strings.Contains(err.Error(), "duplicate key") {
		return 0, errors.New("this tenant already has a target with that name")
	}
	return id, err
}

const targetCols = `st.id, st.tenant_id, t.name, st.name, st.kind, st.url, st.sftp_password, st.sftp_key, st.sftp_host_key, st.created_at`

func scanTarget(r pgx.Row) (Target, error) {
	var t Target
	err := r.Scan(&t.ID, &t.TenantID, &t.TenantName, &t.Name, &t.Kind, &t.URL, &t.SFTPPassword, &t.SFTPKey, &t.SFTPHostKey, &t.CreatedAt)
	return t, err
}

func (s *Store) ListTargets(ctx context.Context) ([]Target, error) {
	rows, err := s.db.Query(ctx, `SELECT `+targetCols+` FROM storage_targets st JOIN tenants t ON t.id=st.tenant_id ORDER BY t.name, st.name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Target, error) { return scanTarget(r) })
}

func (s *Store) GetTarget(ctx context.Context, id int64) (Target, error) {
	t, err := scanTarget(s.db.QueryRow(ctx, `SELECT `+targetCols+` FROM storage_targets st JOIN tenants t ON t.id=st.tenant_id WHERE st.id=$1`, id))
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
func (s *Store) CreateEnrollmentToken(ctx context.Context, tenantID int64, ttl time.Duration) (string, time.Time, error) {
	tok := randomToken(24)
	exp := time.Now().Add(ttl)
	_, err := s.db.Exec(ctx, `INSERT INTO enrollment_tokens(tenant_id, token_hash, expires_at) VALUES($1,$2,$3)`,
		tenantID, hashToken(tok), exp)
	return tok, exp, err
}

// EnrollAgent validates a token and registers a new agent.
func (s *Store) EnrollAgent(ctx context.Context, req api.EnrollRequest) (api.EnrollResponse, error) {
	var tenantID int64
	err := s.db.QueryRow(ctx, `SELECT tenant_id FROM enrollment_tokens WHERE token_hash=$1 AND expires_at > now()`,
		hashToken(req.Token)).Scan(&tenantID)
	if err != nil {
		return api.EnrollResponse{}, errors.New("invalid or expired enrollment token")
	}
	if strings.TrimSpace(req.Hostname) == "" {
		return api.EnrollResponse{}, errors.New("hostname is required")
	}
	resp := api.EnrollResponse{AgentUUID: newUUID(), Secret: randomToken(32)}
	_, err = s.db.Exec(ctx, `INSERT INTO agents(tenant_id, uuid, secret_hash, hostname, os, arch, version, last_seen_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,now())`,
		tenantID, resp.AgentUUID, hashToken(resp.Secret), req.Hostname, req.OS, req.Arch, req.Version)
	return resp, err
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
	TenantID   int64
	TenantName string
	TenantSlug string
	UUID       string
	Hostname   string
	OS         string
	Arch       string
	Version    string
	EnrolledAt time.Time
	LastSeen   *time.Time
}

// Online reports whether the agent polled recently.
func (a Agent) Online() bool {
	return a.LastSeen != nil && time.Since(*a.LastSeen) < 3*time.Minute
}

const agentCols = `a.id, a.tenant_id, t.name, t.slug, a.uuid, a.hostname, a.os, a.arch, a.version, a.enrolled_at, a.last_seen_at`

func scanAgent(r pgx.Row) (Agent, error) {
	var a Agent
	err := r.Scan(&a.ID, &a.TenantID, &a.TenantName, &a.TenantSlug, &a.UUID, &a.Hostname, &a.OS, &a.Arch, &a.Version, &a.EnrolledAt, &a.LastSeen)
	return a, err
}

// AuthenticateAgent checks "uuid:secret" credentials.
func (s *Store) AuthenticateAgent(ctx context.Context, uuid, secret string) (Agent, error) {
	var hash []byte
	a, err := scanAgentWithHash(s.db.QueryRow(ctx, `SELECT `+agentCols+`, a.secret_hash FROM agents a JOIN tenants t ON t.id=a.tenant_id WHERE a.uuid=$1`, uuid), &hash)
	if err != nil {
		return Agent{}, errors.New("unknown agent")
	}
	if subtleEqual(hash, hashToken(secret)) {
		return a, nil
	}
	return Agent{}, errors.New("invalid agent credentials")
}

func scanAgentWithHash(r pgx.Row, hash *[]byte) (Agent, error) {
	var a Agent
	err := r.Scan(&a.ID, &a.TenantID, &a.TenantName, &a.TenantSlug, &a.UUID, &a.Hostname, &a.OS, &a.Arch, &a.Version, &a.EnrolledAt, &a.LastSeen, hash)
	return a, err
}

func (s *Store) TouchAgent(ctx context.Context, id int64, req api.PollRequest) error {
	_, err := s.db.Exec(ctx, `UPDATE agents SET last_seen_at=now(),
		hostname=COALESCE(NULLIF($2,''),hostname), os=COALESCE(NULLIF($3,''),os),
		arch=COALESCE(NULLIF($4,''),arch), version=COALESCE(NULLIF($5,''),version) WHERE id=$1`,
		id, req.Hostname, req.OS, req.Arch, req.Version)
	return err
}

func (s *Store) ListAgents(ctx context.Context) ([]Agent, error) {
	rows, err := s.db.Query(ctx, `SELECT `+agentCols+` FROM agents a JOIN tenants t ON t.id=a.tenant_id ORDER BY t.name, a.hostname`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Agent, error) { return scanAgent(r) })
}

func (s *Store) GetAgent(ctx context.Context, id int64) (Agent, error) {
	a, err := scanAgent(s.db.QueryRow(ctx, `SELECT `+agentCols+` FROM agents a JOIN tenants t ON t.id=a.tenant_id WHERE a.id=$1`, id))
	return a, notFound(err)
}

func (s *Store) DeleteAgent(ctx context.Context, id int64) error {
	_, err := s.db.Exec(ctx, `DELETE FROM agents WHERE id=$1`, id)
	return err
}

// ---- jobs

type Job struct {
	ID         int64
	TenantID   int64
	TenantName string
	AgentID    int64
	Hostname   string
	TargetID   int64
	TargetName string
	Name       string
	Paths      []string
	Excludes   []string
	Schedule   string
	Enabled    bool
	LastSched  *time.Time
	CreatedAt  time.Time
	// Last run summary
	LastStatus   *string
	LastFinished *time.Time
}

func (s *Store) CreateJob(ctx context.Context, j Job) (int64, error) {
	j.Name = strings.TrimSpace(j.Name)
	if j.Name == "" || len(j.Paths) == 0 {
		return 0, errors.New("name and at least one path are required")
	}
	if j.Schedule != "" {
		if _, err := parseSchedule(j.Schedule); err != nil {
			return 0, err
		}
	}
	agent, err := s.GetAgent(ctx, j.AgentID)
	if err != nil {
		return 0, errors.New("unknown agent")
	}
	target, err := s.GetTarget(ctx, j.TargetID)
	if err != nil {
		return 0, errors.New("unknown storage target")
	}
	if agent.TenantID != target.TenantID {
		return 0, errors.New("agent and storage target belong to different tenants")
	}
	if j.Excludes == nil {
		j.Excludes = []string{}
	}
	var id int64
	err = s.db.QueryRow(ctx, `INSERT INTO jobs(tenant_id, agent_id, target_id, name, paths, excludes, schedule, enabled, last_scheduled_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,now()) RETURNING id`,
		agent.TenantID, j.AgentID, j.TargetID, j.Name, j.Paths, j.Excludes, j.Schedule, j.Enabled).Scan(&id)
	return id, err
}

const jobCols = `j.id, j.tenant_id, t.name, j.agent_id, a.hostname, j.target_id, st.name, j.name, j.paths, j.excludes,
	j.schedule, j.enabled, j.last_scheduled_at, j.created_at,
	(SELECT r.status FROM runs r WHERE r.job_id=j.id AND r.kind='backup' ORDER BY r.queued_at DESC LIMIT 1),
	(SELECT r.finished_at FROM runs r WHERE r.job_id=j.id AND r.kind='backup' ORDER BY r.queued_at DESC LIMIT 1)`

const jobFrom = ` FROM jobs j JOIN tenants t ON t.id=j.tenant_id JOIN agents a ON a.id=j.agent_id JOIN storage_targets st ON st.id=j.target_id`

func scanJob(r pgx.Row) (Job, error) {
	var j Job
	err := r.Scan(&j.ID, &j.TenantID, &j.TenantName, &j.AgentID, &j.Hostname, &j.TargetID, &j.TargetName, &j.Name,
		&j.Paths, &j.Excludes, &j.Schedule, &j.Enabled, &j.LastSched, &j.CreatedAt, &j.LastStatus, &j.LastFinished)
	return j, err
}

func (s *Store) ListJobs(ctx context.Context) ([]Job, error) {
	rows, err := s.db.Query(ctx, `SELECT `+jobCols+jobFrom+` ORDER BY t.name, a.hostname, j.name`)
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
		return strings.TrimRight(t.URL, `/\`) + "/" + a.TenantSlug + "/" + a.UUID
	}
	return t.URL + "/" + path.Join(a.TenantSlug, a.UUID)
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
	var id int64
	err = s.db.QueryRow(ctx, `INSERT INTO runs(tenant_id, agent_id, job_id, kind, trigger, repo_url, target_id, paths, excludes)
		SELECT $1,$2,$3,'backup',$4,$5,$6,$7,$8
		WHERE NOT EXISTS (SELECT 1 FROM runs WHERE job_id=$3 AND kind='backup' AND status IN ('queued','running'))
		RETURNING id`,
		j.TenantID, j.AgentID, j.ID, trigger, repoURL(t, a), t.ID, j.Paths, j.Excludes).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrRunActive
	}
	return id, err
}

// QueueRestore creates a restore run from a successful backup run.
// agentID selects the agent that performs the restore (same tenant).
func (s *Store) QueueRestore(ctx context.Context, backupRunID, agentID int64, target string, includes []string, verify bool) (int64, error) {
	b, err := s.GetRun(ctx, backupRunID)
	if err != nil {
		return 0, err
	}
	if b.Kind != api.KindBackup || b.SnapshotID == "" {
		return 0, errors.New("run has no snapshot to restore")
	}
	a, err := s.GetAgent(ctx, agentID)
	if err != nil {
		return 0, errors.New("unknown agent")
	}
	if a.TenantID != b.TenantID {
		return 0, errors.New("agent belongs to a different tenant")
	}
	if includes == nil {
		includes = []string{}
	}
	var id int64
	err = s.db.QueryRow(ctx, `INSERT INTO runs(tenant_id, agent_id, job_id, kind, trigger, repo_url, target_id, paths, snapshot_id, restore_target, restore_verify)
		VALUES($1,$2,$3,'restore','manual',$4,$5,$6,$7,$8,$9) RETURNING id`,
		b.TenantID, agentID, b.JobID, b.RepoURL, b.TargetID, includes, b.SnapshotID, target, verify).Scan(&id)
	return id, err
}

type Run struct {
	ID            int64
	TenantID      int64
	TenantName    string
	AgentID       int64
	Hostname      string
	JobID         *int64
	JobName       *string
	Kind          string
	Status        string
	Trigger       string
	RepoURL       string
	TargetID      *int64
	Paths         []string
	Excludes      []string
	SnapshotID    string
	RestoreTarget string
	RestoreVerify bool
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

const runCols = `r.id, r.tenant_id, t.name, r.agent_id, a.hostname, r.job_id, j.name, r.kind, r.status, r.trigger, r.repo_url,
	r.target_id, r.paths, r.excludes, r.snapshot_id, r.restore_target, r.restore_verify, r.queued_at, r.started_at, r.finished_at,
	r.stats, r.errors, r.message`

const runFrom = ` FROM runs r JOIN tenants t ON t.id=r.tenant_id JOIN agents a ON a.id=r.agent_id LEFT JOIN jobs j ON j.id=r.job_id`

func scanRun(row pgx.Row) (Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.TenantID, &r.TenantName, &r.AgentID, &r.Hostname, &r.JobID, &r.JobName, &r.Kind, &r.Status,
		&r.Trigger, &r.RepoURL, &r.TargetID, &r.Paths, &r.Excludes, &r.SnapshotID, &r.RestoreTarget, &r.RestoreVerify,
		&r.QueuedAt, &r.StartedAt, &r.FinishedAt, &r.Stats, &r.Errors, &r.Message)
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
		WHERE id = (SELECT id FROM runs WHERE agent_id=$1 AND status='queued' ORDER BY queued_at LIMIT 1 FOR UPDATE SKIP LOCKED)
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
	var stats any
	if len(res.Stats) > 0 {
		stats = res.Stats
	}
	ct, err := s.db.Exec(ctx, `UPDATE runs SET status=$3, finished_at=now(), stats=$4, errors=$5, message=$6,
		snapshot_id=CASE WHEN kind='backup' THEN $7 ELSE snapshot_id END
		WHERE id=$2 AND agent_id=$1 AND status='running'`,
		agentID, runID, res.Status, stats, res.Errors, res.Message, res.SnapshotID)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
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

// Dashboard counters.
type Summary struct {
	Tenants, Agents, AgentsOnline, Jobs     int
	Runs24h, Failed24h, Warning24h, Running int
}

func (s *Store) Summary(ctx context.Context) (Summary, error) {
	var x Summary
	err := s.db.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM tenants),
		(SELECT count(*) FROM agents),
		(SELECT count(*) FROM agents WHERE last_seen_at > now() - interval '3 minutes'),
		(SELECT count(*) FROM jobs),
		(SELECT count(*) FROM runs WHERE queued_at > now() - interval '24 hours'),
		(SELECT count(*) FROM runs WHERE queued_at > now() - interval '24 hours' AND status='failed'),
		(SELECT count(*) FROM runs WHERE queued_at > now() - interval '24 hours' AND status='warning'),
		(SELECT count(*) FROM runs WHERE status IN ('queued','running'))`).
		Scan(&x.Tenants, &x.Agents, &x.AgentsOnline, &x.Jobs, &x.Runs24h, &x.Failed24h, &x.Warning24h, &x.Running)
	return x, err
}

// dummyHash is compared against when a username does not exist, so login
// takes the same time whether or not the user exists.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("backupzit-dummy"), bcrypt.DefaultCost)

func subtleEqual(a, b []byte) bool { return subtle.ConstantTimeCompare(a, b) == 1 }

// RandomPassword returns a random password for generated accounts.
func RandomPassword() string { return randomToken(15) }
