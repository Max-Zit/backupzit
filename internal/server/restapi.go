package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// REST API for scripts, RMM and monitoring tools:
//
//	Authorization: Bearer bzt_...
//
// Tokens belong to a user and carry a role no higher than the user's.

// APIToken is a user's API token (the secret is shown only at creation).
type APIToken struct {
	ID        int64
	UserID    int64
	Username  string
	Name      string
	Role      string
	CreatedAt time.Time
	LastUsed  *time.Time
	ExpiresAt *time.Time
}

func (t APIToken) RoleName() string { return roleTitle(t.Role) }

func (t APIToken) Expired() bool { return t.ExpiresAt != nil && time.Now().After(*t.ExpiresAt) }

var roleRank = map[string]int{"viewer": 1, "restore": 2, "operator": 3, "admin": 4}

func (s *Store) CreateAPIToken(ctx context.Context, owner User, name, role string, days int) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return "", errors.New("give the token a name (e.g. the tool that uses it)")
	}
	if _, ok := roleByKey(role); !ok {
		return "", errors.New("choose a role")
	}
	if roleRank[role] > roleRank[owner.Role] {
		return "", errors.New("a token cannot have more rights than your own role")
	}
	if days < 0 || days > 3650 {
		return "", errors.New("expiry must be 0 (never) to 3650 days")
	}
	var exp *time.Time
	if days > 0 {
		t := time.Now().AddDate(0, 0, days)
		exp = &t
	}
	tok := "bzt_" + randomToken(30)
	_, err := s.db.Exec(ctx, `INSERT INTO api_tokens(user_id, name, token_hash, role, expires_at) VALUES($1,$2,$3,$4,$5)`,
		owner.ID, name, hashToken(tok), role, exp)
	return tok, err
}

func (s *Store) ListAPITokens(ctx context.Context, userID int64) ([]APIToken, error) {
	rows, err := s.db.Query(ctx, `SELECT t.id, t.user_id, u.username, t.name, t.role, t.created_at, t.last_used_at, t.expires_at
		FROM api_tokens t JOIN users u ON u.id=t.user_id WHERE ($1=0 OR t.user_id=$1) ORDER BY t.created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (APIToken, error) {
		var t APIToken
		err := r.Scan(&t.ID, &t.UserID, &t.Username, &t.Name, &t.Role, &t.CreatedAt, &t.LastUsed, &t.ExpiresAt)
		return t, err
	})
}

func (s *Store) DeleteAPIToken(ctx context.Context, id, userID int64) error {
	ct, err := s.db.Exec(ctx, `DELETE FROM api_tokens WHERE id=$1 AND ($2=0 OR user_id=$2)`, id, userID)
	if err == nil && ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// tokenUser authenticates an API token; the returned user carries the
// token's role.
func (s *Store) tokenUser(ctx context.Context, token string) (User, string, error) {
	if !strings.HasPrefix(token, "bzt_") {
		return User{}, "", ErrNotFound
	}
	var role, name string
	var id int64
	u, err := scanUserWith(s.db.QueryRow(ctx, `UPDATE api_tokens t SET last_used_at=now() FROM users u
		WHERE u.id=t.user_id AND t.token_hash=$1 AND NOT u.disabled AND (t.expires_at IS NULL OR t.expires_at > now())
		RETURNING `+prefixed("u.", userCols)+`, t.role, t.name, t.id`, hashToken(token)), &role, &name, &id)
	if err != nil {
		return User{}, "", err
	}
	// The token's role, but never more than the user currently has.
	if roleRank[role] < roleRank[u.Role] {
		u.Role = role
	}
	return u, fmt.Sprintf("%s (token %q)", u.Username, name), nil
}

func prefixed(p, cols string) string {
	parts := strings.Split(cols, ",")
	for i, c := range parts {
		parts[i] = p + strings.TrimSpace(c)
	}
	return strings.Join(parts, ", ")
}

func scanUserWith(r pgx.Row, extra ...any) (User, error) {
	var u User
	dest := append([]any{&u.ID, &u.Username, &u.DisplayName, &u.Email, &u.Role, &u.Source, &u.Disabled, &u.LastLogin, &u.CreatedAt, &u.TwoFactor}, extra...)
	return u, notFound(r.Scan(dest...))
}

// ---- HTTP

type apiHandler func(w http.ResponseWriter, r *http.Request) (any, error)

type apiError struct {
	code int
	msg  string
}

func (e apiError) Error() string { return e.msg }

func apiErr(code int, format string, a ...any) error {
	return apiError{code, fmt.Sprintf(format, a...)}
}

// api authenticates a token and checks perm; responses are JSON.
func (s *Server) api(perm Perm, h apiHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		u, who, err := s.store.tokenUser(r.Context(), strings.TrimSpace(token))
		if err != nil {
			time.Sleep(300 * time.Millisecond)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing, invalid or expired API token"})
			return
		}
		if !u.Can(string(perm)) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "the token's role does not allow this"})
			return
		}
		u.Username = who
		r = r.WithContext(context.WithValue(r.Context(), userCtxKey, &u))
		v, err := h(w, r)
		if err != nil {
			var ae apiError
			switch {
			case errors.As(err, &ae):
				writeJSON(w, ae.code, map[string]string{"error": ae.msg})
			case errors.Is(err, ErrNotFound):
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			case errors.Is(err, ErrRunActive):
				writeJSON(w, http.StatusConflict, map[string]string{"error": "a run of this job is already queued or running"})
			default:
				s.log.Error("api", "path", r.URL.Path, "err", err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			}
			return
		}
		code := http.StatusOK
		if r.Method == http.MethodPost {
			code = http.StatusAccepted
		}
		writeJSON(w, code, v)
	}
}

// JSON shapes (stable field names, independent of internal structs).

type apiAgent struct {
	ID       int64      `json:"id"`
	Hostname string     `json:"hostname"`
	OS       string     `json:"os"`
	Version  string     `json:"version"`
	Online   bool       `json:"online"`
	LastSeen *time.Time `json:"last_seen"`
	Recovery bool       `json:"recovery"`
	Enrolled time.Time  `json:"enrolled"`
}

type apiJob struct {
	ID           int64      `json:"id"`
	Name         string     `json:"name"`
	Kind         string     `json:"kind"`
	AgentID      int64      `json:"agent_id"`
	Agent        string     `json:"agent"`
	TargetID     int64      `json:"target_id"`
	Target       string     `json:"target"`
	Enabled      bool       `json:"enabled"`
	Schedule     string     `json:"schedule"`
	NextRun      *time.Time `json:"next_run"`
	LastStatus   string     `json:"last_status"`
	LastFinished *time.Time `json:"last_finished"`
	Paths        []string   `json:"paths,omitempty"`
	SourceJobID  *int64     `json:"source_job_id,omitempty"`
}

type apiRun struct {
	ID         int64           `json:"id"`
	Kind       string          `json:"kind"`
	Status     string          `json:"status"`
	Trigger    string          `json:"trigger"`
	AgentID    int64           `json:"agent_id"`
	Agent      string          `json:"agent"`
	JobID      *int64          `json:"job_id"`
	Job        string          `json:"job"`
	QueuedAt   time.Time       `json:"queued_at"`
	StartedAt  *time.Time      `json:"started_at"`
	FinishedAt *time.Time      `json:"finished_at"`
	DurationS  int             `json:"duration_s"`
	Snapshot   string          `json:"snapshot,omitempty"`
	Message    string          `json:"message,omitempty"`
	Errors     []string        `json:"errors,omitempty"`
	Stats      json.RawMessage `json:"stats,omitempty"`
	Suspicious string          `json:"suspicious,omitempty"`
	Expired    bool            `json:"expired"`
}

func toAPIAgent(a Agent) apiAgent {
	return apiAgent{a.ID, a.Hostname, a.OS + "/" + a.Arch, a.Version, a.Online(), a.LastSeen, a.Recovery, a.EnrolledAt}
}

func toAPIJob(j Job) apiJob {
	x := apiJob{ID: j.ID, Name: j.Name, Kind: j.Kind, AgentID: j.AgentID, Agent: j.Hostname, TargetID: j.TargetID, Target: j.TargetName,
		Enabled: j.Enabled, Schedule: DescribeSchedule(j.Schedule), LastStatus: deref(j.LastStatus), LastFinished: j.LastFinished,
		Paths: j.Paths, SourceJobID: j.SourceJobID}
	if j.Enabled {
		if n := NextRun(j.Schedule, time.Now()); !n.IsZero() {
			x.NextRun = &n
		}
	}
	return x
}

func toAPIRunJSON(r Run) apiRun {
	return apiRun{ID: r.ID, Kind: r.Kind, Status: r.Status, Trigger: r.Trigger, AgentID: r.AgentID, Agent: r.Hostname,
		JobID: r.JobID, Job: deref(r.JobName), QueuedAt: r.QueuedAt, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
		DurationS: int(r.Duration().Seconds()), Snapshot: r.SnapshotID, Message: r.Message, Errors: r.Errors,
		Stats: r.Stats, Suspicious: r.Anomaly, Expired: r.Expired}
}

func pathInt(r *http.Request, name string) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil {
		return 0, apiErr(http.StatusBadRequest, "invalid %s", name)
	}
	return id, nil
}

func (s *Server) registerAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/status", s.api(PermView, func(w http.ResponseWriter, r *http.Request) (any, error) {
		sum, err := s.store.Summary(r.Context())
		return map[string]any{"version": s.Version, "agents": sum.Agents, "agents_online": sum.AgentsOnline, "jobs": sum.Jobs,
			"storage_targets": sum.Targets, "runs_24h": sum.Runs24h, "failed_24h": sum.Failed24h, "warnings_24h": sum.Warning24h,
			"active_runs": sum.Running, "suspicious_unreviewed": sum.Suspicious,
			"ok": sum.Failed24h == 0 && sum.Suspicious == 0 && sum.AgentsOnline == sum.Agents}, err
	}))
	mux.HandleFunc("GET /api/v1/agents", s.api(PermView, func(w http.ResponseWriter, r *http.Request) (any, error) {
		list, err := s.store.ListAgents(r.Context())
		out := []apiAgent{}
		for _, a := range list {
			out = append(out, toAPIAgent(a))
		}
		return out, err
	}))
	mux.HandleFunc("GET /api/v1/jobs", s.api(PermView, func(w http.ResponseWriter, r *http.Request) (any, error) {
		list, err := s.store.ListJobs(r.Context())
		out := []apiJob{}
		for _, j := range list {
			out = append(out, toAPIJob(j))
		}
		return out, err
	}))
	mux.HandleFunc("GET /api/v1/jobs/{id}", s.api(PermView, func(w http.ResponseWriter, r *http.Request) (any, error) {
		id, err := pathInt(r, "id")
		if err != nil {
			return nil, err
		}
		j, err := s.store.GetJob(r.Context(), id)
		if err != nil {
			return nil, err
		}
		return toAPIJob(j), nil
	}))
	mux.HandleFunc("POST /api/v1/jobs/{id}/run", s.api(PermRun, func(w http.ResponseWriter, r *http.Request) (any, error) {
		id, err := pathInt(r, "id")
		if err != nil {
			return nil, err
		}
		if _, err := s.store.GetJob(r.Context(), id); err != nil {
			return nil, err
		}
		rid, err := s.store.QueueBackup(r.Context(), id, "api")
		if err != nil {
			return nil, err
		}
		s.audit(r, "api.job_run", "job %d, run %d", id, rid)
		return map[string]int64{"run_id": rid}, nil
	}))
	mux.HandleFunc("POST /api/v1/jobs/{id}/test", s.api(PermRun, func(w http.ResponseWriter, r *http.Request) (any, error) {
		id, err := pathInt(r, "id")
		if err != nil {
			return nil, err
		}
		rid, err := s.store.QueueRestoreTest(r.Context(), id, "api")
		if err != nil && !errors.Is(err, ErrRunActive) && !errors.Is(err, ErrNotFound) {
			return nil, apiErr(http.StatusConflict, "%s", err.Error())
		}
		if err != nil {
			return nil, err
		}
		s.audit(r, "api.job_test", "job %d, run %d", id, rid)
		return map[string]int64{"run_id": rid}, nil
	}))
	mux.HandleFunc("GET /api/v1/runs", s.api(PermView, func(w http.ResponseWriter, r *http.Request) (any, error) {
		q := r.URL.Query()
		job, _ := strconv.ParseInt(q.Get("job"), 10, 64)
		agent, _ := strconv.ParseInt(q.Get("agent"), 10, 64)
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit <= 0 || limit > 1000 {
			limit = 100
		}
		list, err := s.store.ListRuns(r.Context(), RunFilter{JobID: job, AgentID: agent, Limit: limit})
		out := []apiRun{}
		for _, x := range list {
			if st := q.Get("status"); st != "" && x.Status != st {
				continue
			}
			out = append(out, toAPIRunJSON(x))
		}
		return out, err
	}))
	mux.HandleFunc("GET /api/v1/runs/{id}", s.api(PermView, func(w http.ResponseWriter, r *http.Request) (any, error) {
		id, err := pathInt(r, "id")
		if err != nil {
			return nil, err
		}
		x, err := s.store.GetRun(r.Context(), id)
		if err != nil {
			return nil, err
		}
		return toAPIRunJSON(x), nil
	}))
	mux.HandleFunc("GET /api/v1/report", s.api(PermView, func(w http.ResponseWriter, r *http.Request) (any, error) {
		rep, err := s.reportFromRequest(r)
		if err != nil {
			return nil, apiErr(http.StatusBadRequest, "%s", err.Error())
		}
		jobs := []map[string]any{}
		for _, j := range rep.Jobs {
			jobs = append(jobs, map[string]any{"job_id": j.JobID, "job": j.Name, "agent": j.Hostname, "runs": j.Runs,
				"success": j.Success, "warning": j.Warning, "failed": j.Failed, "success_rate": j.SuccessRate,
				"last_status": j.LastStatus, "last_good_backup": j.LastSuccess, "uploaded_bytes": j.BytesStored,
				"restore_test": j.LastTest, "restore_test_at": j.LastTestAt})
		}
		return map[string]any{"from": rep.Period.From, "to": rep.Period.To, "label": rep.Period.Label, "scope": rep.Scope(),
			"backups": rep.Backups, "success": rep.Success, "warning": rep.Warning, "failed": rep.Failed,
			"success_rate": rep.SuccessRate, "read_bytes": rep.BytesRead, "uploaded_bytes": rep.BytesStored,
			"restores": rep.Restores, "restore_tests": rep.Tests, "restore_tests_failed": rep.TestsFailed,
			"suspicious": rep.Suspicious, "jobs": jobs}, nil
	}))
	mux.HandleFunc("GET /api/v1/targets", s.api(PermView, func(w http.ResponseWriter, r *http.Request) (any, error) {
		list, err := s.store.ListTargets(r.Context())
		out := []map[string]any{}
		for _, t := range list { // no credentials
			out = append(out, map[string]any{"id": t.ID, "name": t.Name, "kind": t.Kind, "location": t.URL,
				"encrypted": t.Encrypted, "immutable_days": t.S3LockDays})
		}
		return out, err
	}))
}

// ---- token management pages (My account)

func (s *Server) handleTokenCreate(w http.ResponseWriter, r *http.Request, user string) {
	me := currentUser(r)
	days, _ := strconv.Atoi(r.FormValue("days"))
	tok, err := s.store.CreateAPIToken(r.Context(), *me, r.FormValue("name"), r.FormValue("role"), days)
	if err != nil {
		redirectErr(w, r, "/account#tokens", err)
		return
	}
	s.audit(r, "api_token.create", "%q, role %s", r.FormValue("name"), r.FormValue("role"))
	s.renderAccount(w, r, user, tok)
}

func (s *Server) handleTokenDelete(w http.ResponseWriter, r *http.Request, _ string) {
	me := currentUser(r)
	id, _ := pathID(r)
	owner := me.ID
	if me.Can(string(PermUsers)) {
		owner = 0 // administrators may revoke any token
	}
	if err := s.store.DeleteAPIToken(r.Context(), id, owner); err != nil {
		redirectErr(w, r, "/account#tokens", errors.New("token not found"))
		return
	}
	s.audit(r, "api_token.delete", "token %d", id)
	back := "/account#tokens"
	if strings.HasPrefix(r.FormValue("back"), "/users") {
		back = "/users"
	}
	redirectMsg(w, r, back, "API token revoked.")
}
