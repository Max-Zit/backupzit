package server

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/backupzit/backupzit/internal/api"
	"github.com/jackc/pgx/v5"
)

// ---- periods

// Period is a reporting interval [From, To).
type Period struct {
	Key   string // preset name or "custom"
	From  time.Time
	To    time.Time
	Label string
}

var periodPresets = []struct{ Key, Name string }{
	{"today", "Today"}, {"yesterday", "Yesterday"}, {"last7", "Last 7 days"}, {"last30", "Last 30 days"},
	{"thisweek", "This week"}, {"lastweek", "Last week"}, {"thismonth", "This month"}, {"lastmonth", "Last month"},
	{"last90", "Last 90 days"}, {"custom", "Custom range"},
}

func midnight(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

func weekStart(t time.Time) time.Time { // Monday
	d := midnight(t)
	return d.AddDate(0, 0, -((int(d.Weekday()) + 6) % 7))
}

// ResolvePeriod turns a preset (or "custom" with from/to dates, to
// inclusive) into a time range in the server's local time.
func ResolvePeriod(key, from, to string, now time.Time) (Period, error) {
	now = now.Local()
	today := midnight(now)
	p := Period{Key: key}
	switch key {
	case "today":
		p.From, p.To = today, today.AddDate(0, 0, 1)
	case "", "yesterday":
		p.Key, p.From, p.To = "yesterday", today.AddDate(0, 0, -1), today
	case "last7":
		p.From, p.To = today.AddDate(0, 0, -6), today.AddDate(0, 0, 1)
	case "last30":
		p.From, p.To = today.AddDate(0, 0, -29), today.AddDate(0, 0, 1)
	case "last90":
		p.From, p.To = today.AddDate(0, 0, -89), today.AddDate(0, 0, 1)
	case "thisweek":
		p.From, p.To = weekStart(now), today.AddDate(0, 0, 1)
	case "lastweek":
		p.To = weekStart(now)
		p.From = p.To.AddDate(0, 0, -7)
	case "thismonth":
		p.From, p.To = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()), today.AddDate(0, 0, 1)
	case "lastmonth":
		p.To = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
		p.From = p.To.AddDate(0, -1, 0)
	case "custom":
		f, err1 := time.ParseInLocation("2006-01-02", from, now.Location())
		t, err2 := time.ParseInLocation("2006-01-02", to, now.Location())
		if err1 != nil || err2 != nil {
			return p, errors.New("choose a start and end date")
		}
		if t.Before(f) {
			f, t = t, f
		}
		if t.Sub(f) > 3*366*24*time.Hour {
			return p, errors.New("a report can cover at most three years")
		}
		p.From, p.To = f, t.AddDate(0, 0, 1)
	default:
		return p, fmt.Errorf("unknown period %q", key)
	}
	last := p.To.AddDate(0, 0, -1)
	if p.From.Equal(last) {
		p.Label = p.From.Format("Mon 2 Jan 2006")
	} else {
		p.Label = p.From.Format("2 Jan 2006") + " – " + last.Format("2 Jan 2006")
	}
	return p, nil
}

// ---- report

// Report summarizes backup activity in a period.
type Report struct {
	Period    Period
	AgentName string
	JobName   string
	Generated time.Time

	Backups, Success, Warning, Failed, Restores int
	Tests, TestsFailed                          int
	Suspicious                                  int
	SuccessRate                                 float64
	BytesRead, BytesStored                      uint64
	Duration                                    time.Duration

	Jobs     []JobReport
	Days     []DayReport
	Problems []Run
	Runs     []Run
	MaxDay   int
}

type JobReport struct {
	JobID                          *int64
	Name, Hostname                 string
	Runs, Success, Warning, Failed int
	LastStatus                     string
	LastRun, LastSuccess           *time.Time
	BytesStored, ProtectedBytes    uint64
	ProtectedFiles                 uint64
	Duration                       time.Duration
	SuccessRate                    float64
	Missed                         bool   // no successful backup in the period
	LastTest                       string // status of the newest restore test
	LastTestAt                     *time.Time
}

type DayReport struct {
	Date                            time.Time
	Success, Warning, Failed, Total int
}

func isBackupKind(k string) bool {
	return k == api.KindBackup || k == api.KindImageBackup || k == api.KindVMBackup || k == api.KindSystemBackup || k == api.KindSQLBackup || k == api.KindSQLLog
}

// ListRunsBetween returns runs queued in [from, to), newest first.
func (s *Store) ListRunsBetween(ctx context.Context, from, to time.Time, agentID, jobID int64) ([]Run, error) {
	rows, err := s.db.Query(ctx, `SELECT `+runCols+runFrom+` WHERE r.queued_at >= $1 AND r.queued_at < $2
		AND ($3=0 OR r.agent_id=$3) AND ($4=0 OR r.job_id=$4) ORDER BY r.queued_at DESC LIMIT 50000`, from, to, agentID, jobID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Run, error) { return scanRun(r) })
}

type runStats struct {
	Files       uint64 `json:"files"`
	Bytes       uint64 `json:"bytes"`
	BytesRead   uint64 `json:"bytes_read"`
	BytesStored uint64 `json:"bytes_stored"`
}

// BuildReport computes the report for a period.
func (s *Store) BuildReport(ctx context.Context, p Period, agentID, jobID int64) (*Report, error) {
	runs, err := s.ListRunsBetween(ctx, p.From, p.To, agentID, jobID)
	if err != nil {
		return nil, err
	}
	rep := &Report{Period: p, Generated: time.Now(), Runs: runs}
	if agentID != 0 {
		if a, err := s.GetAgent(ctx, agentID); err == nil {
			rep.AgentName = a.Hostname
		}
	}
	if jobID != 0 {
		if j, err := s.GetJob(ctx, jobID); err == nil {
			rep.JobName = j.Name
		}
	}
	days := map[string]*DayReport{}
	for d := p.From; d.Before(p.To); d = d.AddDate(0, 0, 1) {
		dr := &DayReport{Date: d}
		days[d.Format("2006-01-02")] = dr
		rep.Days = append(rep.Days, *dr)
	}
	jobs := map[string]*JobReport{}
	tests := map[string]Run{}
	var order []string
	for i := len(runs) - 1; i >= 0; i-- { // oldest first
		r := runs[i]
		if r.Kind == api.KindVerify && r.FinishedAt != nil {
			rep.Tests++
			if r.Status == api.StatusFailed {
				rep.TestsFailed++
				rep.Problems = append([]Run{r}, rep.Problems...)
			}
			tests[fmt.Sprintf("%d/%s", r.AgentID, deref(r.JobName))] = r // oldest first: newest wins
			continue
		}
		if r.Kind == api.KindCopy {
			r.Kind = api.KindBackup // copies count like backups of their job
		}
		if !isBackupKind(r.Kind) {
			if strings.Contains(r.Kind, "restore") {
				rep.Restores++
			}
			continue
		}
		if r.Status != api.StatusSuccess && r.Status != api.StatusWarning && r.Status != api.StatusFailed {
			continue // still queued or running
		}
		rep.Backups++
		var st runStats
		if len(r.Stats) > 0 {
			json.Unmarshal(r.Stats, &st)
		}
		rep.BytesRead += st.BytesRead
		rep.BytesStored += st.BytesStored
		rep.Duration += r.Duration()
		key := fmt.Sprintf("%d/%s", r.AgentID, deref(r.JobName))
		jr := jobs[key]
		if jr == nil {
			jr = &JobReport{JobID: r.JobID, Name: deref(r.JobName), Hostname: r.Hostname}
			if jr.Name == "" {
				jr.Name = "(ad hoc)"
			}
			jobs[key] = jr
			order = append(order, key)
		}
		jr.Runs++
		jr.BytesStored += st.BytesStored
		jr.Duration += r.Duration()
		jr.LastStatus = r.Status
		at := r.QueuedAt
		if r.FinishedAt != nil {
			at = *r.FinishedAt
		}
		jr.LastRun = &at
		dr := days[r.QueuedAt.Local().Format("2006-01-02")]
		switch r.Status {
		case api.StatusSuccess:
			rep.Success++
			jr.Success++
		case api.StatusWarning:
			rep.Warning++
			jr.Warning++
		case api.StatusFailed:
			rep.Failed++
			jr.Failed++
		}
		if r.Status != api.StatusFailed {
			jr.LastSuccess = &at
			jr.ProtectedBytes, jr.ProtectedFiles = st.Bytes, st.Files
		}
		if r.Anomaly != "" {
			rep.Suspicious++
		}
		if r.Status == api.StatusFailed || r.Status == api.StatusWarning || r.Anomaly != "" {
			rep.Problems = append([]Run{r}, rep.Problems...) // newest first
		}
		if dr != nil {
			dr.Total++
			switch r.Status {
			case api.StatusSuccess:
				dr.Success++
			case api.StatusWarning:
				dr.Warning++
			case api.StatusFailed:
				dr.Failed++
			}
		}
	}
	for i := range rep.Days {
		rep.Days[i] = *days[rep.Days[i].Date.Format("2006-01-02")]
		if rep.Days[i].Total > rep.MaxDay {
			rep.MaxDay = rep.Days[i].Total
		}
	}
	if rep.Backups > 0 {
		rep.SuccessRate = 100 * float64(rep.Success+rep.Warning) / float64(rep.Backups)
	}
	for _, k := range order {
		jr := jobs[k]
		jr.SuccessRate = 100 * float64(jr.Success+jr.Warning) / float64(jr.Runs)
		jr.Missed = jr.LastSuccess == nil
		if t, ok := tests[k]; ok {
			jr.LastTest, jr.LastTestAt = t.Status, t.FinishedAt
		}
		rep.Jobs = append(rep.Jobs, *jr)
	}
	sort.Slice(rep.Jobs, func(i, j int) bool {
		if rep.Jobs[i].Hostname != rep.Jobs[j].Hostname {
			return rep.Jobs[i].Hostname < rep.Jobs[j].Hostname
		}
		return rep.Jobs[i].Name < rep.Jobs[j].Name
	})
	if len(rep.Problems) > 200 {
		rep.Problems = rep.Problems[:200]
	}
	return rep, nil
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Scope describes the report's filters for titles.
func (rep *Report) Scope() string {
	switch {
	case rep.JobName != "":
		return "job " + rep.JobName
	case rep.AgentName != "":
		return "agent " + rep.AgentName
	}
	return "all agents"
}

// CSV lists every backup run of the report.
func (rep *Report) CSV() []byte {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	w.Write([]string{"run", "type", "agent", "job", "status", "queued", "started", "finished", "duration_s", "files", "size_bytes", "read_bytes", "uploaded_bytes", "message"})
	for i := len(rep.Runs) - 1; i >= 0; i-- {
		r := rep.Runs[i]
		var st runStats
		if len(r.Stats) > 0 {
			json.Unmarshal(r.Stats, &st)
		}
		ts := func(t *time.Time) string {
			if t == nil {
				return ""
			}
			return t.Local().Format("2006-01-02 15:04:05")
		}
		w.Write([]string{strconv.FormatInt(r.ID, 10), r.Kind, r.Hostname, csvSafe(deref(r.JobName)), r.Status,
			r.QueuedAt.Local().Format("2006-01-02 15:04:05"), ts(r.StartedAt), ts(r.FinishedAt),
			strconv.Itoa(int(r.Duration().Seconds())), strconv.FormatUint(st.Files, 10), strconv.FormatUint(st.Bytes, 10),
			strconv.FormatUint(st.BytesRead, 10), strconv.FormatUint(st.BytesStored, 10), csvSafe(r.Message)})
	}
	w.Flush()
	return b.Bytes()
}

// csvSafe defuses spreadsheet formulas in values that come from agents or
// users ("CSV injection").
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// Text renders the report for email.
func (rep *Report) Text(consoleURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "BackupZit report: %s (%s)\n\n", rep.Period.Label, rep.Scope())
	fmt.Fprintf(&b, "Backups:       %d (success %d, warnings %d, failed %d)\n", rep.Backups, rep.Success, rep.Warning, rep.Failed)
	fmt.Fprintf(&b, "Success rate:  %.1f %%\n", rep.SuccessRate)
	fmt.Fprintf(&b, "Data read:     %s\nUploaded:      %s\nRestores:      %d\n\n", humanBytes(rep.BytesRead), humanBytes(rep.BytesStored), rep.Restores)
	if len(rep.Jobs) > 0 {
		b.WriteString("Jobs\n----\n")
		for _, j := range rep.Jobs {
			last := "never"
			if j.LastSuccess != nil {
				last = j.LastSuccess.Local().Format("2006-01-02 15:04")
			}
			fmt.Fprintf(&b, "%-28s %-20s runs %3d  ok %3d  warn %3d  failed %3d  last good: %s\n",
				trunc(j.Name, 28), trunc(j.Hostname, 20), j.Runs, j.Success, j.Warning, j.Failed, last)
		}
		b.WriteString("\n")
	}
	if len(rep.Problems) > 0 {
		b.WriteString("Problems\n--------\n")
		for i, r := range rep.Problems {
			if i == 30 {
				fmt.Fprintf(&b, "... and %d more (see the attached CSV)\n", len(rep.Problems)-30)
				break
			}
			fmt.Fprintf(&b, "#%d %s %s %s on %s: %s\n", r.ID, r.QueuedAt.Local().Format("2006-01-02 15:04"), strings.ToUpper(r.Status),
				deref(r.JobName), r.Hostname, trunc(r.Message, 200))
		}
		b.WriteString("\n")
	}
	if consoleURL != "" {
		fmt.Fprintf(&b, "Console: %s/reports\n", strings.TrimRight(consoleURL, "/"))
	}
	return b.String()
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func (rep *Report) Subject() string {
	state := "OK"
	if rep.Failed > 0 {
		state = "ATTENTION"
	} else if rep.Warning > 0 {
		state = "WARNINGS"
	}
	return fmt.Sprintf("[BackupZit] Report %s (%s): %s", rep.Period.Label, rep.Scope(), state)
}

func (rep *Report) FileName() string {
	return fmt.Sprintf("backupzit-report-%s-%s.csv", rep.Period.From.Format("20060102"), rep.Period.To.AddDate(0, 0, -1).Format("20060102"))
}

// ---- web

func (s *Server) reportFromRequest(r *http.Request) (*Report, error) {
	q := r.URL.Query()
	if r.Method == http.MethodPost {
		q = r.PostForm
	}
	p, err := ResolvePeriod(q.Get("period"), q.Get("from"), q.Get("to"), time.Now())
	if err != nil {
		return nil, err
	}
	agentID, _ := strconv.ParseInt(q.Get("agent"), 10, 64)
	jobID, _ := strconv.ParseInt(q.Get("job"), 10, 64)
	return s.store.BuildReport(r.Context(), p, agentID, jobID)
}

func (s *Server) handleReports(w http.ResponseWriter, r *http.Request, user string) {
	agents, err := s.store.ListAgents(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	jobs, err := s.store.ListJobs(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	schedules, err := s.store.ListReportSchedules(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	pd := pageData{Title: "Reports", Nav: "reports", User: user}
	rep, err := s.reportFromRequest(r)
	if err != nil {
		pd.Error = err.Error()
	}
	q := r.URL.Query()
	if q.Get("period") == "" {
		q.Set("period", "last7")
		if rep2, err := s.reportFromRequest(&http.Request{Method: http.MethodGet, URL: &url.URL{RawQuery: q.Encode()}}); err == nil {
			rep2.Generated = time.Now()
			rep = rep2
		}
	}
	pd.Data = map[string]any{"Report": rep, "Agents": agents, "Jobs": jobs, "Presets": periodPresets, "Q": q,
		"Query": q.Encode(), "Schedules": schedules}
	s.render(w, r, "reports", pd)
}

func (s *Server) handleReportCSV(w http.ResponseWriter, r *http.Request, _ string) {
	rep, err := s.reportFromRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+rep.FileName()+`"`)
	w.Write(rep.CSV())
}

func (s *Server) handleReportEmail(w http.ResponseWriter, r *http.Request, _ string) {
	r.ParseForm()
	back := "/reports?" + r.PostForm.Encode()
	rep, err := s.reportFromRequest(r)
	if err != nil {
		redirectErr(w, r, back, err)
		return
	}
	var e EmailSettings
	if err := s.store.GetSetting(r.Context(), settingEmail, &e); err != nil || e.Host == "" || len(e.To) == 0 {
		redirectErr(w, r, back, errors.New("configure email under Settings first"))
		return
	}
	if to := splitAddrs(r.PostForm.Get("to")); len(to) > 0 {
		e.To = to
	}
	if err := checkAddrs(e.To); err != nil {
		redirectErr(w, r, back, err)
		return
	}
	if err := sendMailWith(r.Context(), e, rep.Subject(), rep.Text(s.consoleURL()),
		[]attachment{{Name: rep.FileName(), ContentType: "text/csv; charset=utf-8", Data: rep.CSV()}}); err != nil {
		redirectErr(w, r, back, fmt.Errorf("send report: %w", err))
		return
	}
	redirectMsg(w, r, back, "Report sent to "+strings.Join(e.To, ", ")+".")
}

func (s *Server) consoleURL() string { return s.PublicURL }

func checkAddrs(list []string) error {
	for _, a := range list {
		if _, err := mail.ParseAddress(a); err != nil || strings.ContainsAny(a, "<>\r\n") {
			return fmt.Errorf("invalid email address %q", a)
		}
	}
	return nil
}

// ---- scheduled reports

type ReportSchedule struct {
	ID         int64
	Name       string
	Frequency  string // daily | weekly | monthly
	AgentID    *int64
	JobID      *int64
	AgentName  *string
	JobName    *string
	Recipients []string
	Hour       int
	LastPeriod string
}

// periodFor returns the completed period a schedule reports on at now.
func (rs ReportSchedule) periodFor(now time.Time) (Period, string) {
	key := map[string]string{"daily": "yesterday", "weekly": "lastweek", "monthly": "lastmonth"}[rs.Frequency]
	p, _ := ResolvePeriod(key, "", "", now)
	return p, rs.Frequency + ":" + p.From.Format("2006-01-02")
}

func (rs ReportSchedule) Describe() string {
	switch rs.Frequency {
	case "daily":
		return fmt.Sprintf("Every day at %02d:00, covering the previous day", rs.Hour)
	case "weekly":
		return fmt.Sprintf("Every Monday at %02d:00, covering the previous week", rs.Hour)
	case "monthly":
		return fmt.Sprintf("On the 1st of every month at %02d:00, covering the previous month", rs.Hour)
	}
	return rs.Frequency
}

func (s *Store) ListReportSchedules(ctx context.Context) ([]ReportSchedule, error) {
	rows, err := s.db.Query(ctx, `SELECT rs.id, rs.name, rs.frequency, rs.agent_id, rs.job_id, a.hostname, j.name, rs.recipients, rs.hour, rs.last_period
		FROM report_schedules rs LEFT JOIN agents a ON a.id=rs.agent_id LEFT JOIN jobs j ON j.id=rs.job_id ORDER BY rs.name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ReportSchedule, error) {
		var x ReportSchedule
		err := r.Scan(&x.ID, &x.Name, &x.Frequency, &x.AgentID, &x.JobID, &x.AgentName, &x.JobName, &x.Recipients, &x.Hour, &x.LastPeriod)
		return x, err
	})
}

func (s *Store) CreateReportSchedule(ctx context.Context, x ReportSchedule) (int64, error) {
	x.Name = strings.TrimSpace(x.Name)
	if x.Name == "" {
		return 0, errors.New("name is required")
	}
	if x.Frequency != "daily" && x.Frequency != "weekly" && x.Frequency != "monthly" {
		return 0, errors.New("choose daily, weekly or monthly")
	}
	if x.Hour < 0 || x.Hour > 23 {
		return 0, errors.New("hour must be 0-23")
	}
	if err := checkAddrs(x.Recipients); err != nil {
		return 0, err
	}
	if x.Recipients == nil {
		x.Recipients = []string{}
	}
	// The first report is the next completed period, not the current one.
	_, key := x.periodFor(time.Now())
	var id int64
	err := s.db.QueryRow(ctx, `INSERT INTO report_schedules(name, frequency, agent_id, job_id, recipients, hour, last_period)
		VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id`, x.Name, x.Frequency, x.AgentID, x.JobID, x.Recipients, x.Hour, key).Scan(&id)
	return id, err
}

func (s *Store) DeleteReportSchedule(ctx context.Context, id int64) error {
	_, err := s.db.Exec(ctx, `DELETE FROM report_schedules WHERE id=$1`, id)
	return err
}

func optionalID(v string) *int64 {
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || id <= 0 {
		return nil
	}
	return &id
}

func (s *Server) handleReportScheduleCreate(w http.ResponseWriter, r *http.Request, _ string) {
	hour, _ := strconv.Atoi(r.FormValue("hour"))
	x := ReportSchedule{Name: r.FormValue("name"), Frequency: r.FormValue("frequency"), Hour: hour,
		AgentID: optionalID(r.FormValue("agent")), JobID: optionalID(r.FormValue("job")), Recipients: splitAddrs(r.FormValue("recipients"))}
	if _, err := s.store.CreateReportSchedule(r.Context(), x); err != nil {
		redirectErr(w, r, "/reports#scheduled", err)
		return
	}
	redirectMsg(w, r, "/reports", "Scheduled report created.")
}

func (s *Server) handleReportScheduleDelete(w http.ResponseWriter, r *http.Request, _ string) {
	id, _ := pathID(r)
	if err := s.store.DeleteReportSchedule(r.Context(), id); err != nil {
		redirectErr(w, r, "/reports", err)
		return
	}
	redirectMsg(w, r, "/reports", "Scheduled report removed.")
}

// scheduledReports sends reports whose period has completed.
func (n *Notifier) scheduledReports(ctx context.Context, e EmailSettings, now time.Time) {
	list, err := n.store.ListReportSchedules(ctx)
	if err != nil {
		n.log.Error("report schedules", "err", err)
		return
	}
	for _, rs := range list {
		p, key := rs.periodFor(now)
		if key == rs.LastPeriod || now.Local().Hour() < rs.Hour {
			continue
		}
		var agentID, jobID int64
		if rs.AgentID != nil {
			agentID = *rs.AgentID
		}
		if rs.JobID != nil {
			jobID = *rs.JobID
		}
		rep, err := n.store.BuildReport(ctx, p, agentID, jobID)
		if err != nil {
			n.log.Error("build scheduled report", "report", rs.Name, "err", err)
			continue
		}
		to := e
		if len(rs.Recipients) > 0 {
			to.To = rs.Recipients
		}
		if err := n.sendWith(ctx, to, rep.Subject(), rep.Text(n.publicURL()),
			[]attachment{{Name: rep.FileName(), ContentType: "text/csv; charset=utf-8", Data: rep.CSV()}}); err != nil {
			n.log.Error("send scheduled report", "report", rs.Name, "err", err)
			continue
		}
		n.store.db.Exec(ctx, `UPDATE report_schedules SET last_period=$2 WHERE id=$1`, rs.ID, key)
		n.log.Info("scheduled report sent", "report", rs.Name, "period", p.Label)
	}
}

// Bar is one stacked column of the daily chart, in SVG units (viewBox
// 0 0 1000 200, bars grow up from y=180).
type Bar struct {
	X, W              float64
	Ok, Warn, Fail    float64 // heights
	OkY, WarnY, FailY float64
	Label, Title      string
	ShowLabel         bool
}

func (rep *Report) Bars() []Bar {
	n := len(rep.Days)
	if n == 0 {
		return nil
	}
	slot := 1000.0 / float64(n)
	w := slot * 0.7
	max := float64(rep.MaxDay)
	if max == 0 {
		max = 1
	}
	every := (n + 13) / 14 // at most ~14 labels
	var out []Bar
	for i, d := range rep.Days {
		b := Bar{X: float64(i)*slot + (slot-w)/2, W: w}
		b.Ok = 160 * float64(d.Success) / max
		b.Warn = 160 * float64(d.Warning) / max
		b.Fail = 160 * float64(d.Failed) / max
		b.OkY = 180 - b.Ok
		b.WarnY = b.OkY - b.Warn
		b.FailY = b.WarnY - b.Fail
		b.Label = d.Date.Format("2 Jan")
		b.ShowLabel = i%every == 0
		b.Title = fmt.Sprintf("%s: %d ok, %d warnings, %d failed", d.Date.Format("Mon 2 Jan"), d.Success, d.Warning, d.Failed)
		out = append(out, b)
	}
	return out
}
