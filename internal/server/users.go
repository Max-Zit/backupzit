package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

// ---- roles and permissions

// Perm is something a role allows.
type Perm string

const (
	PermView     Perm = "view"     // dashboards, activity, calendar, reports, docs
	PermRun      Perm = "run"      // start a backup of an existing job
	PermRestore  Perm = "restore"  // restore files and disk images
	PermJobs     Perm = "jobs"     // create, change, delete jobs; report schedules
	PermAgents   Perm = "agents"   // enroll and remove agents, recovery codes
	PermStorage  Perm = "storage"  // storage targets, credentials, recovery keys
	PermSettings Perm = "settings" // email, sessions, LDAP
	PermUsers    Perm = "users"    // users and the audit log
)

// Role is a fixed set of permissions.
type Role struct {
	Key, Name, Description string
	Perms                  []Perm
}

var Roles = []Role{
	{"admin", "Administrator", "Full access, including users, settings, storage credentials and recovery keys.",
		[]Perm{PermView, PermRun, PermRestore, PermJobs, PermAgents, PermStorage, PermSettings, PermUsers}},
	{"operator", "Backup operator", "Manages agents, jobs and schedules, runs backups and restores. Cannot see storage credentials or recovery keys, or change users and settings.",
		[]Perm{PermView, PermRun, PermRestore, PermJobs, PermAgents}},
	{"restore", "Restore operator", "Help desk: sees everything, restores files and disks and can start backups, but cannot change jobs.",
		[]Perm{PermView, PermRun, PermRestore}},
	{"viewer", "Viewer", "Read-only: dashboard, activity, calendar, reports and documentation.",
		[]Perm{PermView}},
}

// AllPerms lists permissions for the roles overview.
var AllPerms = []struct {
	Perm Perm
	Name string
}{
	{PermView, "View dashboards, activity, reports"}, {PermRun, "Start backups"}, {PermRestore, "Restore files and disks"},
	{PermJobs, "Manage jobs and report schedules"}, {PermAgents, "Enroll and remove agents"},
	{PermStorage, "Storage targets and recovery keys"}, {PermSettings, "Settings and LDAP"}, {PermUsers, "Users and audit log"},
}

func roleByKey(k string) (Role, bool) {
	for _, r := range Roles {
		if r.Key == k {
			return r, true
		}
	}
	return Role{}, false
}

func (r Role) Has(p Perm) bool {
	for _, x := range r.Perms {
		if x == p {
			return true
		}
	}
	return false
}

// ---- users

type User struct {
	ID          int64
	Username    string
	DisplayName string
	Email       string
	Role        string
	Source      string // local | ldap
	Disabled    bool
	LastLogin   *time.Time
	TwoFactor   bool
	CreatedAt   time.Time
}

func (u *User) Can(p string) bool {
	if u == nil || u.Disabled {
		return false
	}
	r, ok := roleByKey(u.Role)
	return ok && r.Has(Perm(p))
}

func (u User) RoleName() string {
	if r, ok := roleByKey(u.Role); ok {
		return r.Name
	}
	return u.Role
}

func (u User) Name() string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Username
}

const userCols = `id, username, display_name, email, role, source, disabled, last_login_at, created_at, totp_enabled`

func scanUser(r pgx.Row) (User, error) {
	var u User
	err := r.Scan(&u.ID, &u.Username, &u.DisplayName, &u.Email, &u.Role, &u.Source, &u.Disabled, &u.LastLogin, &u.CreatedAt, &u.TwoFactor)
	return u, notFound(err)
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.Query(ctx, `SELECT `+userCols+` FROM users ORDER BY lower(username)`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (User, error) { return scanUser(r) })
}

func (s *Store) GetUser(ctx context.Context, id int64) (User, error) {
	return scanUser(s.db.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id=$1`, id))
}

func (s *Store) userByName(ctx context.Context, username string) (User, string, error) {
	var hash string
	var u User
	err := s.db.QueryRow(ctx, `SELECT `+userCols+`, password_hash FROM users WHERE lower(username)=lower($1)`, username).
		Scan(&u.ID, &u.Username, &u.DisplayName, &u.Email, &u.Role, &u.Source, &u.Disabled, &u.LastLogin, &u.CreatedAt, &u.TwoFactor, &hash)
	return u, hash, notFound(err)
}

var validUsername = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,63}$`)

// MinPasswordLength applies to local accounts.
const MinPasswordLength = 10

func checkPassword(pw string) error {
	if len([]rune(pw)) < MinPasswordLength {
		return fmt.Errorf("the password must have at least %d characters", MinPasswordLength)
	}
	if len(pw) > 72 {
		return errors.New("the password may have at most 72 bytes")
	}
	return nil
}

func (s *Store) CreateUser(ctx context.Context, u User, password string) (int64, error) {
	u.Username = strings.TrimSpace(u.Username)
	if !validUsername.MatchString(u.Username) {
		return 0, errors.New("username may contain letters, digits, '.', '_', '-' and '@' (max. 64)")
	}
	if _, ok := roleByKey(u.Role); !ok {
		return 0, errors.New("choose a role")
	}
	if u.Email = strings.TrimSpace(u.Email); u.Email != "" {
		if _, err := mail.ParseAddress(u.Email); err != nil {
			return 0, errors.New("invalid email address")
		}
	}
	if err := checkPassword(password); err != nil {
		return 0, err
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.db.QueryRow(ctx, `INSERT INTO users(username, password_hash, display_name, email, role, source)
		VALUES($1,$2,$3,$4,$5,'local') RETURNING id`, u.Username, string(h), strings.TrimSpace(u.DisplayName), u.Email, u.Role).Scan(&id)
	if err != nil && strings.Contains(err.Error(), "duplicate key") {
		return 0, errors.New("a user with that name already exists")
	}
	return id, err
}

// UpdateUser changes profile, role and status. Disabling signs the user out.
func (s *Store) UpdateUser(ctx context.Context, u User) error {
	if _, ok := roleByKey(u.Role); !ok {
		return errors.New("choose a role")
	}
	if u.Email = strings.TrimSpace(u.Email); u.Email != "" {
		if _, err := mail.ParseAddress(u.Email); err != nil {
			return errors.New("invalid email address")
		}
	}
	_, err := s.db.Exec(ctx, `UPDATE users SET display_name=$2, email=$3, role=$4, disabled=$5 WHERE id=$1`,
		u.ID, strings.TrimSpace(u.DisplayName), u.Email, u.Role, u.Disabled)
	if err == nil && u.Disabled {
		_, err = s.db.Exec(ctx, `DELETE FROM sessions WHERE user_id=$1`, u.ID)
	}
	return err
}

// SetUserPassword sets a local user's password and signs out its sessions.
func (s *Store) SetUserPassword(ctx context.Context, id int64, password string) error {
	if err := checkPassword(password); err != nil {
		return err
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	tag, err := s.db.Exec(ctx, `UPDATE users SET password_hash=$2 WHERE id=$1 AND source='local'`, id, string(h))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("only local accounts have a password here; LDAP passwords are changed in the directory")
	}
	_, err = s.db.Exec(ctx, `DELETE FROM sessions WHERE user_id=$1`, id)
	return err
}

func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	_, err := s.db.Exec(ctx, `DELETE FROM users WHERE id=$1`, id)
	return err
}

// checkAdminsRemain makes sure a change leaves at least one enabled local
// administrator, so the console cannot be locked out (e.g. if LDAP is down).
func (s *Store) checkAdminsRemain(ctx context.Context, changed User) error {
	var n int
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM users WHERE role='admin' AND source='local' AND NOT disabled AND id<>$1`, changed.ID).Scan(&n)
	if err != nil {
		return err
	}
	if n == 0 && !(changed.Role == "admin" && changed.Source == "local" && !changed.Disabled) {
		return errors.New("at least one enabled local administrator must remain")
	}
	return nil
}

// upsertLDAPUser records an LDAP user after a successful sign-in.
func (s *Store) upsertLDAPUser(ctx context.Context, username, display, email, role string) (User, error) {
	return s.upsertDirectoryUser(ctx, "ldap", username, display, email, role)
}

// upsertDirectoryUser records a user of a directory ("ldap") or an identity
// provider ("sso") after a successful sign-in; a local account (or one of
// the other source) with the same name is left alone.
func (s *Store) upsertDirectoryUser(ctx context.Context, source, username, display, email, role string) (User, error) {
	_, err := s.db.Exec(ctx, `INSERT INTO users(username, password_hash, display_name, email, role, source)
		VALUES($1,'!' || $5,$2,$3,$4,$5)
		ON CONFLICT (lower(username)) DO UPDATE SET display_name=EXCLUDED.display_name, email=EXCLUDED.email, role=EXCLUDED.role
		WHERE users.source=EXCLUDED.source`, strings.ToLower(username), display, email, role, source)
	if err != nil {
		return User{}, err
	}
	u, _, err := s.userByName(ctx, username)
	return u, err
}

// newSession creates a session for an authenticated user.
func (s *Store) newSession(ctx context.Context, userID int64, ttl time.Duration) (string, error) {
	s.db.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now()`)
	tok := randomToken(32)
	_, err := s.db.Exec(ctx, `INSERT INTO sessions(token_hash, user_id, expires_at) VALUES($1,$2,$3)`,
		hashToken(tok), userID, time.Now().Add(ttl))
	if err == nil {
		s.db.Exec(ctx, `UPDATE users SET last_login_at=now() WHERE id=$1`, userID)
	}
	return tok, err
}

// SessionUser returns the user of a valid session and records the
// activity. idle > 0 ends sessions unused for that long.
func (s *Store) SessionUser(ctx context.Context, token string, idle time.Duration) (User, error) {
	return scanUser(s.db.QueryRow(ctx, `UPDATE sessions s SET last_seen_at=now() FROM users u
		WHERE u.id=s.user_id AND s.token_hash=$1 AND s.expires_at > now() AND NOT u.disabled
		AND ($2::bigint = 0 OR s.last_seen_at > now() - make_interval(secs => $2::bigint))
		RETURNING u.id, u.username, u.display_name, u.email, u.role, u.source, u.disabled, u.last_login_at, u.created_at, u.totp_enabled`,
		hashToken(token), int64(idle.Seconds())))
}

var errBadLogin = errors.New("invalid username or password")

// authenticate checks a password against the local account or LDAP.
func (s *Server) authenticate(ctx context.Context, username, password string) (User, error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" || len(username) > 256 || len(password) > 1024 {
		return User{}, errBadLogin
	}
	u, hash, err := s.store.userByName(ctx, username)
	if err == nil && u.Source == "local" {
		if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
			return User{}, errBadLogin
		}
		if u.Disabled {
			return User{}, errors.New("this account is disabled")
		}
		return u, nil
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		return User{}, err
	}
	var ls LDAPSettings
	if s.store.GetSetting(ctx, settingLDAP, &ls) == nil && ls.Enabled {
		res, lerr := ls.Authenticate(ctx, username, password)
		if lerr != nil {
			if !errors.Is(lerr, errBadLogin) {
				s.log.Warn("LDAP sign-in failed", "user", username, "err", lerr)
			}
			if errors.Is(lerr, errNoRole) {
				return User{}, lerr
			}
			return User{}, errBadLogin
		}
		lu, err := s.store.upsertLDAPUser(ctx, username, res.DisplayName, res.Email, res.Role)
		if err != nil {
			return User{}, err
		}
		if lu.Source != "ldap" {
			return User{}, errBadLogin
		}
		if lu.Disabled {
			return User{}, errors.New("this account is disabled")
		}
		return lu, nil
	}
	bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
	return User{}, errBadLogin
}

// ---- request context

const userCtxKey ctxKey = 2

func currentUser(r *http.Request) *User {
	u, _ := r.Context().Value(userCtxKey).(*User)
	return u
}

// ---- audit log

type AuditEntry struct {
	ID                               int64
	At                               time.Time
	Username, Action, Detail, Remote string
	PrevHash, Hash                   string
}

func (s *Server) audit(r *http.Request, action, format string, args ...any) {
	user := ""
	if u := currentUser(r); u != nil {
		user = u.Username
	}
	s.auditAs(r, user, action, fmt.Sprintf(format, args...))
}

func (s *Server) auditAs(r *http.Request, user, action, detail string) {
	if _, err := s.store.AppendAudit(r.Context(), user, action, detail, s.guard.ClientIP(r)); err != nil {
		s.log.Error("audit log", "err", err)
	}
	s.log.Info("audit", "user", user, "action", action, "detail", detail, "remote", s.guard.ClientIP(r))
}

func (s *Store) ListAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	rows, err := s.db.Query(ctx, `SELECT at, username, action, detail, remote FROM audit_log ORDER BY at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AuditEntry, error) {
		var a AuditEntry
		err := r.Scan(&a.At, &a.Username, &a.Action, &a.Detail, &a.Remote)
		return a, err
	})
}

// ---- pages

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request, user string) {
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	var ls LDAPSettings
	s.store.GetSetting(r.Context(), settingLDAP, &ls)
	tokens, err := s.store.ListAPITokens(r.Context(), 0)
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, r, "users", pageData{Title: "Users", Nav: "users", User: user, Data: map[string]any{
		"Users": users, "Roles": Roles, "Perms": AllPerms, "LDAP": ls.Enabled, "MinPassword": MinPasswordLength, "Tokens": tokens}})
}

func (s *Server) handleUserCreate(w http.ResponseWriter, r *http.Request, _ string) {
	if r.FormValue("password") != r.FormValue("password2") {
		redirectErr(w, r, "/users", errors.New("the passwords do not match"))
		return
	}
	u := User{Username: r.FormValue("username"), DisplayName: r.FormValue("display_name"), Email: r.FormValue("email"), Role: r.FormValue("role")}
	if _, err := s.store.CreateUser(r.Context(), u, r.FormValue("password")); err != nil {
		redirectErr(w, r, "/users", err)
		return
	}
	s.audit(r, "user.create", "%s (%s)", u.Username, u.Role)
	redirectMsg(w, r, "/users", "User "+u.Username+" created.")
}

func (s *Server) userFromPath(w http.ResponseWriter, r *http.Request) (User, bool) {
	id, err := pathID(r)
	if err != nil {
		http.NotFound(w, r)
		return User{}, false
	}
	u, err := s.store.GetUser(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return u, false
	} else if err != nil {
		s.serverError(w, err)
		return u, false
	}
	return u, true
}

func (s *Server) handleUser(w http.ResponseWriter, r *http.Request, user string) {
	u, ok := s.userFromPath(w, r)
	if !ok {
		return
	}
	s.render(w, r, "user", pageData{Title: "User " + u.Username, Nav: "users", User: user, Data: map[string]any{
		"U": u, "Roles": Roles, "Self": u.ID == currentUser(r).ID, "MinPassword": MinPasswordLength}})
}

func (s *Server) handleUserUpdate(w http.ResponseWriter, r *http.Request, _ string) {
	u, ok := s.userFromPath(w, r)
	if !ok {
		return
	}
	back := fmt.Sprintf("/users/%d", u.ID)
	old := u
	u.DisplayName, u.Email, u.Role = r.FormValue("display_name"), r.FormValue("email"), r.FormValue("role")
	u.Disabled = r.FormValue("disabled") == "on"
	if u.ID == currentUser(r).ID && (u.Disabled || u.Role != old.Role) {
		redirectErr(w, r, back, errors.New("you cannot change your own role or disable yourself"))
		return
	}
	if err := s.store.checkAdminsRemain(r.Context(), u); err != nil && old.Role == "admin" && old.Source == "local" && !old.Disabled {
		redirectErr(w, r, back, err)
		return
	}
	if err := s.store.UpdateUser(r.Context(), u); err != nil {
		redirectErr(w, r, back, err)
		return
	}
	s.audit(r, "user.update", "%s: role %s→%s, disabled %v", u.Username, old.Role, u.Role, u.Disabled)
	redirectMsg(w, r, back, "User saved.")
}

func (s *Server) handleUserPassword(w http.ResponseWriter, r *http.Request, _ string) {
	u, ok := s.userFromPath(w, r)
	if !ok {
		return
	}
	back := fmt.Sprintf("/users/%d", u.ID)
	if r.FormValue("password") != r.FormValue("password2") {
		redirectErr(w, r, back, errors.New("the passwords do not match"))
		return
	}
	if err := s.store.SetUserPassword(r.Context(), u.ID, r.FormValue("password")); err != nil {
		redirectErr(w, r, back, err)
		return
	}
	s.audit(r, "user.password", "password of %s reset", u.Username)
	redirectMsg(w, r, back, "Password changed; the user was signed out everywhere.")
}

// checkUserDeletable keeps the last enabled local administrator.
func (s *Store) checkUserDeletable(ctx context.Context, u User) error {
	gone := u
	gone.Disabled = true
	if err := s.checkAdminsRemain(ctx, gone); err != nil && u.Role == "admin" && u.Source == "local" && !u.Disabled {
		return err
	}
	return nil
}

// deleteUserChecked deletes a user unless it is the last enabled local
// administrator.
func (s *Store) deleteUserChecked(ctx context.Context, u User) error {
	if err := s.checkUserDeletable(ctx, u); err != nil {
		return err
	}
	return s.DeleteUser(ctx, u.ID)
}

func (s *Server) handleUserDelete(w http.ResponseWriter, r *http.Request, _ string) {
	u, ok := s.userFromPath(w, r)
	if !ok {
		return
	}
	if u.ID == currentUser(r).ID {
		redirectErr(w, r, fmt.Sprintf("/users/%d", u.ID), errors.New("you cannot delete yourself"))
		return
	}
	if err := s.store.checkUserDeletable(r.Context(), u); err != nil {
		redirectErr(w, r, fmt.Sprintf("/users/%d", u.ID), err)
		return
	}
	if s.needsApproval(w, r, "/users", "user.delete", u.ID, u.Username, nil) {
		return
	}
	if err := s.store.DeleteUser(r.Context(), u.ID); err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, "user.delete", "%s", u.Username)
	redirectMsg(w, r, "/users", "User "+u.Username+" deleted.")
}

func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request, user string) {
	s.renderAccount(w, r, user, "")
}

func (s *Server) renderAccount(w http.ResponseWriter, r *http.Request, user, newToken string) {
	me := currentUser(r)
	role, _ := roleByKey(me.Role)
	tokens, err := s.store.ListAPITokens(r.Context(), me.ID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	var roles []Role
	for _, x := range Roles {
		if roleRank[x.Key] <= roleRank[me.Role] {
			roles = append(roles, x)
		}
	}
	s.render(w, r, "account", pageData{Title: "My account", Nav: "account", User: user, Data: map[string]any{
		"U": me, "Role": role, "Perms": AllPerms, "MinPassword": MinPasswordLength, "Tokens": tokens, "TokenRoles": roles,
		"NewToken": newToken, "APIURL": s.PublicURL}})
}

func (s *Server) handleAccountPassword(w http.ResponseWriter, r *http.Request, _ string) {
	me := currentUser(r)
	if _, err := s.authenticate(r.Context(), me.Username, r.FormValue("current")); err != nil || me.Source != "local" {
		redirectErr(w, r, "/account", errors.New("the current password is wrong"))
		return
	}
	if r.FormValue("password") != r.FormValue("password2") {
		redirectErr(w, r, "/account", errors.New("the new passwords do not match"))
		return
	}
	if err := s.store.SetUserPassword(r.Context(), me.ID, r.FormValue("password")); err != nil {
		redirectErr(w, r, "/account", err)
		return
	}
	s.audit(r, "account.password", "changed own password")
	http.Redirect(w, r, "/login", http.StatusSeeOther) // all sessions ended
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request, user string) {
	list, err := s.store.ListAudit(r.Context(), 500)
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, r, "audit", pageData{Title: "Audit log", Nav: "audit", User: user, Data: map[string]any{"Entries": list, "Integrity": s.auditState.Load()}})
}
