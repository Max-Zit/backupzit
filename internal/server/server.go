// Package server implements the BackupZit management console: the web UI
// for administrators and the HTTPS API that agents poll.
package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/backupzit/backupzit/internal/api"
	"github.com/backupzit/backupzit/internal/imaging"
	"github.com/backupzit/backupzit/internal/repo"
	"github.com/backupzit/backupzit/internal/restorer"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

const sessionCookie = "bz_session"

// Server is the management console.
type Server struct {
	store  *Store
	logins *loginLimiter
	log    *slog.Logger
	// CertFingerprint is shown in enrollment instructions.
	CertFingerprint string
	// PublicURL is how agents reach the server, e.g. https://backup.example.com:8443
	PublicURL string
	// DistDir holds agent installers offered for download.
	DistDir      string
	PollInterval int
	Version      string

	pages map[string]*template.Template
	// Notifier is triggered when a run finishes (optional).
	Notifier  *Notifier
	cache     *repoCache
	sessCache sessionCache
	clock     func() time.Time // replaceable in tests
}

// New creates a server.
func New(store *Store, log *slog.Logger) (*Server, error) {
	s := &Server{store: store, log: log, PollInterval: 30, Version: "dev", cache: newRepoCache(), logins: newLoginLimiter(), clock: time.Now}
	if err := s.loadTemplates(); err != nil {
		return nil, err
	}
	return s, nil
}

var funcs = template.FuncMap{
	"ago": func(t any) string {
		var tm time.Time
		switch v := t.(type) {
		case time.Time:
			tm = v
		case *time.Time:
			if v == nil {
				return "never"
			}
			tm = *v
		default:
			return ""
		}
		d := time.Since(tm)
		switch {
		case d < time.Minute:
			return "just now"
		case d < time.Hour:
			return fmt.Sprintf("%d min ago", int(d.Minutes()))
		case d < 48*time.Hour:
			return fmt.Sprintf("%d h ago", int(d.Hours()))
		}
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	},
	"ts": func(t any) string {
		switch v := t.(type) {
		case time.Time:
			return v.Local().Format("2006-01-02 15:04:05")
		case *time.Time:
			if v == nil {
				return "—"
			}
			return v.Local().Format("2006-01-02 15:04:05")
		}
		return ""
	},
	"bytes": humanBytes,
	"join":  strings.Join,
	"days":  days,
	"sub":   func(a, b int) int { return a - b },
	"pct":   func(f float64) string { return fmt.Sprintf("%.1f", f) },
	"fx":    func(f float64) string { return fmt.Sprintf("%.1f", f) },
	"dur": func(d time.Duration) string {
		if d < time.Minute {
			return d.Round(time.Second).String()
		}
		return d.Round(time.Minute).String()
	},
	"initial": func(s string) string {
		for _, r := range s {
			return strings.ToUpper(string(r))
		}
		return "?"
	},
	"deref": func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	},
	"short": func(s string) string {
		if len(s) > 8 {
			return s[:8]
		}
		return s
	},
	"nextrun": func(stored string) string {
		t := NextRun(stored, time.Now())
		if t.IsZero() {
			return "—"
		}
		return t.Local().Format("Mon 2006-01-02 15:04")
	},
	"schedule": DescribeSchedule,
	"imagesel": DescribeImageSelection,
	"bytes64":  func(n int64) string { return humanBytes(uint64(n)) },
	"upper":    strings.ToUpper,
	"deref2": func(p *int) int {
		if p == nil {
			return 0
		}
		return *p
	},
	"partkind": func(gpt string, mbr uint8) string {
		return imaging.Partition{GPTType: gpt, MBRType: mbr}.Kind()
	},
	"kindtitle": func(k string) string {
		return map[string]string{"backup": "Backup", "restore": "Restore", "image-backup": "Image backup", "image-restore": "Image restore", "image-file-restore": "File restore from image", "copy": "Backup copy", "verify": "Restore test"}[k]
	},
	"hours": func() []int {
		h := make([]int, 24)
		for i := range h {
			h[i] = i
		}
		return h
	},
	"monthdays": func() []int {
		d := make([]int, 28)
		for i := range d {
			d[i] = i + 1
		}
		return d
	},
	// weekdays in display order, Monday first.
	"weekdays": func() []struct {
		Num  int
		Name string
	} {
		return []struct {
			Num  int
			Name string
		}{{1, "Mon"}, {2, "Tue"}, {3, "Wed"}, {4, "Thu"}, {5, "Fri"}, {6, "Sat"}, {0, "Sun"}}
	},
}

// scheduleFromForm builds a schedule from the job form fields.
func scheduleFromForm(r *http.Request) (string, error) {
	sc := Schedule{Kind: r.FormValue("sched_kind")}
	if sc.Kind == "" {
		sc.Kind = SchedManual
	}
	for _, d := range r.Form["sched_days"] {
		if n, err := strconv.Atoi(d); err == nil {
			sc.Days = append(sc.Days, n)
		}
	}
	switch sc.Kind {
	case SchedDaily:
		for _, t := range r.Form["sched_time"] {
			if t = strings.TrimSpace(t); t != "" {
				sc.Times = append(sc.Times, t)
			}
		}
		if len(sc.Days) == 0 {
			return "", errors.New("select at least one day")
		}
	case SchedInterval:
		sc.EveryMinutes, _ = strconv.Atoi(r.FormValue("sched_every"))
		if r.FormValue("sched_window") == "on" {
			sc.FromHour, _ = strconv.Atoi(r.FormValue("sched_from"))
			sc.ToHour, _ = strconv.Atoi(r.FormValue("sched_to"))
		}
		if len(sc.Days) == 0 {
			return "", errors.New("select at least one day")
		}
	case SchedMonthly:
		sc.DayOfMonth, _ = strconv.Atoi(r.FormValue("sched_dom"))
		sc.Times = []string{r.FormValue("sched_month_time")}
	}
	if err := sc.Validate(); err != nil {
		return "", err
	}
	return sc.Encode(), nil
}

func (s *Server) loadTemplates() error {
	s.pages = map[string]*template.Template{}
	entries, err := fs.ReadDir(templateFS, "templates")
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if name == "layout.html" {
			continue
		}
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/"+name)
		if err != nil {
			return fmt.Errorf("template %s: %w", name, err)
		}
		s.pages[strings.TrimSuffix(name, ".html")] = t
	}
	return nil
}

// Handler returns the HTTP handler for UI and agent API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Agent API
	mux.HandleFunc("POST "+api.PathEnroll, s.handleEnroll)
	mux.HandleFunc("POST "+api.PathPoll, s.agentAuth(s.handlePoll))
	mux.HandleFunc("POST "+api.PathRunsPrefix+"{id}/finish", s.agentAuth(s.handleRunFinish))

	// UI
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("GET /login/2fa", s.handleLogin2FA)
	mux.HandleFunc("POST /login/2fa", s.handleLogin2FA)
	mux.HandleFunc("POST /logout", s.ui("", s.handleLogout))
	mux.HandleFunc("GET /{$}", s.ui(PermView, s.handleDashboard))
	mux.HandleFunc("GET /agents", s.ui(PermView, s.handleAgents))
	mux.HandleFunc("POST /agents/token", s.ui(PermAgents, s.handleAgentToken))
	mux.HandleFunc("POST /agents/{id}/delete", s.ui(PermAgents, s.handleAgentDelete))
	mux.HandleFunc("GET /targets", s.ui(PermView, s.handleTargets))
	mux.HandleFunc("POST /targets", s.ui(PermStorage, s.handleTargetCreate))
	mux.HandleFunc("POST /targets/{id}/delete", s.ui(PermStorage, s.handleTargetDelete))
	mux.HandleFunc("POST /targets/{id}/recovery-key", s.ui(PermStorage, s.handleRecoveryKey))
	mux.HandleFunc("POST /targets/{id}/recovery-sheet", s.ui(PermStorage, s.handleRecoverySheet))
	mux.HandleFunc("GET /jobs", s.ui(PermView, s.handleJobs))
	mux.HandleFunc("POST /jobs", s.ui(PermJobs, s.handleJobCreate))
	mux.HandleFunc("GET /jobs/{id}", s.ui(PermView, s.handleJob))
	mux.HandleFunc("POST /jobs/{id}/run", s.ui(PermRun, s.handleJobRun))
	mux.HandleFunc("POST /jobs/{id}/test", s.ui(PermRun, s.handleJobTest))
	mux.HandleFunc("POST /jobs/{id}/enable", s.ui(PermJobs, s.handleJobEnable(true)))
	mux.HandleFunc("POST /jobs/{id}/disable", s.ui(PermJobs, s.handleJobEnable(false)))
	mux.HandleFunc("POST /jobs/{id}/delete", s.ui(PermJobs, s.handleJobDelete))
	mux.HandleFunc("GET /runs", s.ui(PermView, s.handleRuns))
	mux.HandleFunc("GET /runs/{id}", s.ui(PermView, s.handleRun))
	mux.HandleFunc("POST /runs/{id}/restore", s.ui(PermRestore, s.handleRestore))
	mux.HandleFunc("GET /runs/{id}/browse", s.ui(PermRestore, s.handleBrowse))
	mux.HandleFunc("POST /runs/{id}/files-restore", s.ui(PermRestore, s.handleFilesRestore))
	mux.HandleFunc("GET /downloads/{file}", s.ui(PermView, s.handleDownload))
	mux.HandleFunc("GET /reports", s.ui(PermView, s.handleReports))
	mux.HandleFunc("GET /reports/csv", s.ui(PermView, s.handleReportCSV))
	mux.HandleFunc("POST /reports/email", s.ui(PermJobs, s.handleReportEmail))
	mux.HandleFunc("POST /reports/schedules", s.ui(PermJobs, s.handleReportScheduleCreate))
	mux.HandleFunc("POST /reports/schedules/{id}/delete", s.ui(PermJobs, s.handleReportScheduleDelete))
	mux.HandleFunc("GET /calendar", s.ui(PermView, s.handleCalendar))
	mux.HandleFunc("GET /docs", s.ui(PermView, s.handleDocs))
	mux.HandleFunc("GET /docs/{page}", s.ui(PermView, s.handleDocs))
	mux.HandleFunc("POST /settings/ldap", s.ui(PermSettings, s.handleSettingsLDAP))
	mux.HandleFunc("GET /users", s.ui(PermUsers, s.handleUsers))
	mux.HandleFunc("POST /users", s.ui(PermUsers, s.handleUserCreate))
	mux.HandleFunc("GET /users/{id}", s.ui(PermUsers, s.handleUser))
	mux.HandleFunc("POST /users/{id}", s.ui(PermUsers, s.handleUserUpdate))
	mux.HandleFunc("POST /users/{id}/password", s.ui(PermUsers, s.handleUserPassword))
	mux.HandleFunc("POST /users/{id}/delete", s.ui(PermUsers, s.handleUserDelete))
	mux.HandleFunc("GET /audit", s.ui(PermUsers, s.handleAudit))
	mux.HandleFunc("GET /account", s.ui("", s.handleAccount))
	mux.HandleFunc("POST /account/password", s.ui("", s.handleAccountPassword))
	mux.HandleFunc("GET /account/2fa", s.ui("", s.handleAccount2FA))
	mux.HandleFunc("POST /account/2fa/enable", s.ui("", s.handleAccount2FAEnable))
	mux.HandleFunc("POST /account/2fa/codes", s.ui("", s.handleAccount2FACodes))
	mux.HandleFunc("POST /account/2fa/disable", s.ui("", s.handleAccount2FADisable))
	mux.HandleFunc("POST /users/{id}/2fa-reset", s.ui(PermUsers, s.handleUser2FAReset))
	mux.HandleFunc("GET /recovery", s.ui(PermView, s.handleRecovery))
	mux.HandleFunc("GET /settings", s.ui(PermSettings, s.handleSettings))
	mux.HandleFunc("GET /settings/{tab}", s.ui(PermSettings, s.handleSettings))
	mux.HandleFunc("POST /settings/email", s.ui(PermSettings, s.handleSettingsEmail))
	mux.HandleFunc("POST /settings/sessions", s.ui(PermSettings, s.handleSettingsSessions))
	mux.HandleFunc("POST /settings/tests", s.ui(PermSettings, s.handleSettingsRestoreTests))
	mux.HandleFunc("POST /recovery/token", s.ui(PermAgents, s.handleRecoveryToken))
	mux.HandleFunc("POST /recovery/recovery.json", s.ui(PermAgents, s.handleRecoveryJSON))

	return securityHeaders(mux)
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.ServeHTTP(w, r)
	})
}

// ---- UI plumbing

type pageData struct {
	Title   string
	Nav     string
	User    string
	Flash   string
	Error   string
	Version string
	Data    any
	// Me is the signed-in user (set by render).
	Me *User
}

// ui requires a signed-in user whose role has perm, and rejects
// cross-site form posts.
func (s *Server) ui(perm Perm, h func(w http.ResponseWriter, r *http.Request, user string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		u, err := s.store.SessionUser(r.Context(), c.Value, s.sessionSettings(r.Context()).Idle())
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost && !sameOrigin(r) {
			http.Error(w, "cross-site request rejected", http.StatusForbidden)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), userCtxKey, &u))
		if !u.TwoFactor && s.sessionSettings(r.Context()).mustUse2FA(&u) && !strings.HasPrefix(r.URL.Path, "/account") && r.URL.Path != "/logout" && !strings.HasPrefix(r.URL.Path, "/docs") {
			redirectErr(w, r, "/account/2fa", errors.New("set up two-factor authentication to continue — it is required for your account"))
			return
		}
		if perm != "" && !u.Can(string(perm)) {
			s.log.Warn("permission denied", "user", u.Username, "role", u.Role, "path", r.URL.Path)
			w.WriteHeader(http.StatusForbidden)
			s.render(w, r, "forbidden", pageData{Title: "Not allowed", User: u.Username})
			return
		}
		h(w, r, u.Username)
	}
}

func sameOrigin(r *http.Request) bool {
	src := r.Header.Get("Origin")
	if src == "" || src == "null" {
		src = r.Header.Get("Referer")
	}
	if src == "" {
		return false
	}
	u, err := url.Parse(src)
	return err == nil && u.Host == r.Host
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, d pageData) {
	t, ok := s.pages[page]
	if !ok {
		http.Error(w, "unknown page", http.StatusInternalServerError)
		return
	}
	// Messages from redirects are signed, so a crafted link cannot make the
	// console display arbitrary text.
	q := r.URL.Query()
	if d.Flash == "" && validFlash(q.Get("msg"), q.Get("sig")) {
		d.Flash = q.Get("msg")
	}
	if d.Error == "" && validFlash(q.Get("err"), q.Get("sig")) {
		d.Error = q.Get("err")
	}
	if d.Me == nil {
		d.Me = currentUser(r)
	}
	if d.User != "" {
		w.Header().Set("Cache-Control", "no-store")
	}
	d.Version = s.Version
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.Execute(w, d); err != nil {
		s.log.Error("render", "page", page, "err", err)
	}
}

func redirectMsg(w http.ResponseWriter, r *http.Request, to, msg string) {
	redirectFlash(w, r, to, "msg", msg)
}

func redirectErr(w http.ResponseWriter, r *http.Request, to string, err error) {
	redirectFlash(w, r, to, "err", err.Error())
}

func redirectFlash(w http.ResponseWriter, r *http.Request, to, kind, text string) {
	sep := "?"
	if strings.Contains(to, "?") {
		sep = "&"
	}
	v := url.Values{kind: {text}, "sig": {flashSig(text)}}
	http.Redirect(w, r, to+sep+v.Encode(), http.StatusSeeOther)
}

// flashKey signs redirect messages; it changes on every start, which only
// hides messages of links from before a restart.
var flashKey = func() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}()

func flashSig(text string) string {
	m := hmac.New(sha256.New, flashKey)
	m.Write([]byte(text))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:16])
}

func validFlash(text, sig string) bool {
	return text != "" && hmac.Equal([]byte(sig), []byte(flashSig(text)))
}

func (s *Server) serverError(w http.ResponseWriter, err error) {
	s.log.Error("request failed", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func pathID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

func formID(r *http.Request, name string) int64 {
	id, _ := strconv.ParseInt(r.FormValue(name), 10, 64)
	return id
}

// lines splits a textarea into trimmed, non-empty lines.
func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// ---- auth pages

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "login", pageData{Title: "Sign in"})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-site request rejected", http.StatusForbidden)
		return
	}
	username := r.FormValue("username")
	keys := loginKeys(r, username)
	if wait := s.logins.Blocked(keys); wait > 0 {
		s.log.Warn("sign-in refused: too many failed attempts", "user", username, "remote", r.RemoteAddr)
		s.render(w, r, "login", pageData{Title: "Sign in",
			Error: fmt.Sprintf("Too many failed sign-ins. Try again in %d minutes.", int(wait.Minutes())+1)})
		return
	}
	u, err := s.authenticate(r.Context(), username, r.FormValue("password"))
	if err != nil {
		s.logins.Fail(keys)
		s.log.Warn("failed login", "user", username, "remote", r.RemoteAddr, "err", err)
		s.auditAs(r, clip(username, 64), "login.failed", "")
		msg := errBadLogin.Error()
		if errors.Is(err, errNoRole) {
			msg = err.Error()
		}
		s.render(w, r, "login", pageData{Title: "Sign in", Error: msg})
		return
	}
	if u.TwoFactor {
		// Failure counters are reset only after the second factor.
		http.SetCookie(w, &http.Cookie{Name: mfaCookie, Value: mfaToken(u.ID, s.clock().Add(5*time.Minute)), Path: "/login",
			HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: 300})
		http.Redirect(w, r, "/login/2fa", http.StatusSeeOther)
		return
	}
	s.startSession(w, r, u, "password")
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request, _ string) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.store.Logout(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---- dashboard

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request, user string) {
	sum, err := s.store.Summary(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	runs, err := s.store.ListRuns(r.Context(), RunFilter{Limit: 15})
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, r, "dashboard", pageData{Title: "Dashboard", Nav: "dashboard", User: user,
		Data: map[string]any{"Summary": sum, "Runs": runs}})
}

// ---- agents

type download struct {
	Name, Size string
}

func (s *Server) downloads() []download {
	var out []download
	if s.DistDir == "" {
		return nil
	}
	entries, err := os.ReadDir(s.DistDir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && fi.Mode().IsRegular() {
			out = append(out, download{Name: e.Name(), Size: humanBytes(uint64(fi.Size()))})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

type enrollInfo struct {
	Token       string
	Expires     time.Time
	ServerURL   string
	Fingerprint string
}

func (s *Server) agentsPage(w http.ResponseWriter, r *http.Request, user string, enroll *enrollInfo) {
	agents, err := s.store.ListAgents(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, r, "agents", pageData{Title: "Agents", Nav: "agents", User: user, Data: map[string]any{
		"Agents": agents, "Enroll": enroll, "Downloads": s.downloads(),
	}})
}

func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request, user string) {
	s.agentsPage(w, r, user, nil)
}

func (s *Server) handleAgentToken(w http.ResponseWriter, r *http.Request, user string) {
	tok, exp, err := s.store.CreateEnrollmentToken(r.Context(), 7*24*time.Hour)
	if err != nil {
		s.serverError(w, err)
		return
	}
	pub := s.PublicURL
	if pub == "" {
		pub = "https://" + r.Host
	}
	s.agentsPage(w, r, user, &enrollInfo{Token: tok, Expires: exp, ServerURL: pub, Fingerprint: s.CertFingerprint})
}

func (s *Server) handleAgentDelete(w http.ResponseWriter, r *http.Request, _ string) {
	id, _ := pathID(r)
	if err := s.store.DeleteAgent(r.Context(), id); err != nil {
		redirectErr(w, r, "/agents", err)
		return
	}
	s.audit(r, "agent.delete", "agent %d", id)
	redirectMsg(w, r, "/agents", "Agent removed. Its backups remain on the storage target.")
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request, _ string) {
	name := r.PathValue("file")
	if s.DistDir == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(name))
	http.ServeFile(w, r, filepath.Join(s.DistDir, name))
}

// ---- targets

func (s *Server) handleTargets(w http.ResponseWriter, r *http.Request, user string) {
	targets, err := s.store.ListTargets(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, r, "targets", pageData{Title: "Storage", Nav: "targets", User: user,
		Data: map[string]any{"Targets": targets}})
}

func (s *Server) handleTargetCreate(w http.ResponseWriter, r *http.Request, _ string) {
	t := Target{
		Name:         r.FormValue("name"),
		Kind:         r.FormValue("kind"),
		URL:          r.FormValue("url"),
		SFTPPassword: r.FormValue("sftp_password"),
		SFTPKey:      strings.TrimSpace(r.FormValue("sftp_key")),
		SFTPHostKey:  r.FormValue("sftp_host_key"),
		Encrypted:    r.FormValue("encrypted") == "on",
	}
	if t.Kind == "s3" {
		u, err := s3Location(r.FormValue("s3_endpoint"), r.FormValue("s3_bucket"), r.FormValue("s3_prefix"), r.FormValue("s3_http") == "on")
		if err != nil {
			redirectErr(w, r, "/targets", err)
			return
		}
		t.URL = u
		t.S3AccessKey = r.FormValue("s3_access_key")
		t.S3SecretKey = r.FormValue("s3_secret_key")
		t.S3Region = r.FormValue("s3_region")
		if r.FormValue("s3_immutable") == "on" {
			t.S3LockDays = atoiDefault(r.FormValue("s3_lock_days"))
			if t.S3LockDays < 1 || t.S3LockDays > 3650 {
				redirectErr(w, r, "/targets", errors.New("immutability period must be 1 to 3650 days"))
				return
			}
		}
	}
	if t.Kind == "hardened" {
		u, err := hardenedLocation(r.FormValue("hardened_host"), r.FormValue("hardened_path"))
		if err != nil {
			redirectErr(w, r, "/targets", err)
			return
		}
		t.URL = u
		t.HardenedKey = strings.TrimSpace(r.FormValue("hardened_key"))
		t.HardenedFingerprint = strings.TrimSpace(r.FormValue("hardened_fingerprint"))
	}
	if t.Kind == "smb" {
		u, err := smbLocation(r.FormValue("smb_host"), r.FormValue("smb_share"), r.FormValue("smb_path"), r.FormValue("smb_user"))
		if err != nil {
			redirectErr(w, r, "/targets", err)
			return
		}
		t.URL = u
		t.SMBPassword = r.FormValue("smb_password")
		t.SMBDomain = r.FormValue("smb_domain")
	}
	_, err := s.store.CreateTarget(r.Context(), t)
	if err != nil {
		redirectErr(w, r, "/targets", err)
		return
	}
	s.audit(r, "storage.create", "%s (%s) %s", t.Name, t.Kind, t.URL)
	redirectMsg(w, r, "/targets", "Storage target added.")
}

func (s *Server) handleTargetDelete(w http.ResponseWriter, r *http.Request, _ string) {
	id, _ := pathID(r)
	if err := s.store.DeleteTarget(r.Context(), id); err != nil {
		redirectErr(w, r, "/targets", err)
		return
	}
	s.audit(r, "storage.delete", "target %d", id)
	redirectMsg(w, r, "/targets", "Storage target removed. Data on the storage was not deleted.")
}

// ---- jobs

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request, user string) {
	ctx := r.Context()
	jobs, err := s.store.ListJobs(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	agents, err := s.store.ListAgents(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	targets, err := s.store.ListTargets(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, r, "jobs", pageData{Title: "Backup jobs", Nav: "jobs", User: user,
		Data: map[string]any{"Jobs": jobs, "Agents": agents, "Targets": targets, "Inventory": inventories(agents), "SourceJobs": sourceJobs(jobs)}})
}

func (s *Server) handleJobCreate(w http.ResponseWriter, r *http.Request, _ string) {
	r.ParseForm()
	sched, err := scheduleFromForm(r)
	if err != nil {
		redirectErr(w, r, "/jobs", err)
		return
	}
	job := Job{
		Kind: r.FormValue("kind"),
		Retention: repo.RetentionPolicy{
			KeepLast:    atoiDefault(r.FormValue("keep_last")),
			KeepDaily:   atoiDefault(r.FormValue("keep_daily")),
			KeepWeekly:  atoiDefault(r.FormValue("keep_weekly")),
			KeepMonthly: atoiDefault(r.FormValue("keep_monthly")),
		},
		AgentID:  formID(r, "agent_id"),
		TargetID: formID(r, "target_id"),
		Name:     r.FormValue("name"),
		Paths:    lines(r.FormValue("paths")),
		Excludes: lines(r.FormValue("excludes")),
		Schedule: sched,
		Enabled:  true,
	}
	if job.Kind == JobCopy {
		job.SourceJobID = optionalID(r.FormValue("source_job"))
		job.Paths, job.Excludes = nil, nil
	}
	if job.Kind == JobImage {
		if d, err := strconv.Atoi(r.FormValue("image_disk")); err == nil {
			job.ImageDisk = &d
			job.ImagePartitions = s.partitionSelection(r, formID(r, "agent_id"), d)
		}
	}
	id, err := s.store.CreateJob(r.Context(), job)
	if err != nil {
		redirectErr(w, r, "/jobs", err)
		return
	}
	s.audit(r, "job.create", "#%d %s", id, job.Name)
	redirectMsg(w, r, fmt.Sprintf("/jobs/%d", id), "Job created.")
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request, user string) {
	id, err := pathID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	j, err := s.store.GetJob(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		s.serverError(w, err)
		return
	}
	runs, err := s.store.ListRuns(r.Context(), RunFilter{JobID: id, Limit: 50})
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, r, "job", pageData{Title: j.Name, Nav: "jobs", User: user, Data: map[string]any{"Job": j, "Runs": runs}})
}

func (s *Server) handleJobRun(w http.ResponseWriter, r *http.Request, _ string) {
	id, _ := pathID(r)
	back := fmt.Sprintf("/jobs/%d", id)
	if _, err := s.store.QueueBackup(r.Context(), id, "manual"); err != nil {
		redirectErr(w, r, back, err)
		return
	}
	redirectMsg(w, r, back, "Backup queued. The agent picks it up on its next poll.")
}

func (s *Server) handleJobEnable(enabled bool) func(http.ResponseWriter, *http.Request, string) {
	return func(w http.ResponseWriter, r *http.Request, _ string) {
		id, _ := pathID(r)
		back := fmt.Sprintf("/jobs/%d", id)
		if err := s.store.SetJobEnabled(r.Context(), id, enabled); err != nil {
			redirectErr(w, r, back, err)
			return
		}
		redirectMsg(w, r, back, map[bool]string{true: "Job enabled.", false: "Job disabled."}[enabled])
	}
}

func (s *Server) handleJobDelete(w http.ResponseWriter, r *http.Request, _ string) {
	id, _ := pathID(r)
	if err := s.store.DeleteJob(r.Context(), id); err != nil {
		redirectErr(w, r, "/jobs", err)
		return
	}
	s.audit(r, "job.delete", "job %d", id)
	redirectMsg(w, r, "/jobs", "Job deleted. Existing backups remain on the storage target.")
}

// ---- runs

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request, user string) {
	runs, err := s.store.ListRuns(r.Context(), RunFilter{Limit: 200})
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, r, "runs", pageData{Title: "Activity", Nav: "runs", User: user, Data: runs})
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request, user string) {
	id, err := pathID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	run, err := s.store.GetRun(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		s.serverError(w, err)
		return
	}
	var backupStats *repo.SnapshotStats
	var restoreStats *restorer.Stats
	var copyStats *CopyRunStats
	var testStats *restorer.SampleStats
	if len(run.Stats) > 0 {
		if run.Kind == api.KindBackup || run.Kind == api.KindImageBackup {
			backupStats = &repo.SnapshotStats{}
			json.Unmarshal(run.Stats, backupStats)
		} else if run.Kind == api.KindVerify {
			testStats = &restorer.SampleStats{}
			json.Unmarshal(run.Stats, testStats)
		} else if run.Kind == api.KindCopy {
			copyStats = &CopyRunStats{}
			json.Unmarshal(run.Stats, copyStats)
		} else if run.Kind == api.KindRestore || run.Kind == api.KindImageFileRestore {
			restoreStats = &restorer.Stats{}
			json.Unmarshal(run.Stats, restoreStats)
		}
	}
	agents, err := s.store.ListAgents(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, r, "run", pageData{Title: fmt.Sprintf("Run #%d", run.ID), Nav: "runs", User: user, Data: map[string]any{
		"Run": run, "BackupStats": backupStats, "RestoreStats": restoreStats, "CopyStats": copyStats, "TestStats": testStats, "CopyOfFiles": s.store.copyOfFiles(r.Context(), run), "Agents": agents,
		"Image": imageDetails(run), "Inventory": inventories(agents),
	}})
}

func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request, _ string) {
	id, _ := pathID(r)
	back := fmt.Sprintf("/runs/%d", id)
	if r.FormValue("kind") == "image" {
		s.handleImageRestore(w, r, id, back)
		return
	}
	target := strings.TrimSpace(r.FormValue("target"))
	if r.FormValue("mode") == "original" {
		target = ""
	} else if target == "" {
		redirectErr(w, r, back, errors.New("enter a folder to restore into, or choose original location"))
		return
	}
	rid, err := s.store.QueueRestore(r.Context(), id, formID(r, "agent_id"), target,
		lines(r.FormValue("includes")), r.FormValue("verify") == "on")
	if err != nil {
		redirectErr(w, r, back, err)
		return
	}
	s.audit(r, "restore.files", "run #%d from backup #%d to %q", rid, id, target)
	redirectMsg(w, r, fmt.Sprintf("/runs/%d", rid), "Restore queued.")
}

func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// Shutdown helper for graceful stop.
func Shutdown(srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}
