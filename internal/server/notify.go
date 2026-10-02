package server

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/backupzit/backupzit/internal/api"
	"github.com/jackc/pgx/v5"
)

// EmailSettings configure notifications. Stored under settings key "email".
type EmailSettings struct {
	Enabled  bool     `json:"enabled"`
	Host     string   `json:"host"`
	Port     int      `json:"port"`
	Security string   `json:"security"` // "starttls", "tls" or "none"
	Username string   `json:"username,omitempty"`
	Password string   `json:"password,omitempty"`
	From     string   `json:"from"`
	To       []string `json:"to"`

	OnFailure bool `json:"on_failure"`
	OnWarning bool `json:"on_warning"`
	OnSuccess bool `json:"on_success"`
	// DailyReport sends a summary of the last 24 hours at DailyHour.
	DailyReport bool `json:"daily_report"`
	DailyHour   int  `json:"daily_hour"`
}

const settingEmail = "email"
const settingDailySent = "daily_report_sent" // "2006-01-02" of the last report

// GetSetting loads a JSON setting into v; a missing key leaves v unchanged.
func (s *Store) GetSetting(ctx context.Context, key string, v any) error {
	var b []byte
	err := s.db.QueryRow(ctx, `SELECT value FROM settings WHERE key=$1`, key).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if b, err = s.openSetting(key, b); err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// SetSetting stores v as JSON.
func (s *Store) SetSetting(ctx context.Context, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if b, err = s.sealSetting(key, b); err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `INSERT INTO settings(key, value, updated_at) VALUES($1,$2,now())
		ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, updated_at=now()`, key, b)
	return err
}

// Validate checks settings before they are saved or used.
func (e *EmailSettings) Validate() error {
	if !e.Enabled {
		return nil
	}
	e.Host = strings.TrimSpace(e.Host)
	e.From = strings.TrimSpace(e.From)
	if e.Host == "" || e.From == "" || len(e.To) == 0 {
		return errors.New("SMTP server, sender and at least one recipient are required")
	}
	if e.Port <= 0 || e.Port > 65535 {
		return errors.New("invalid SMTP port")
	}
	switch e.Security {
	case "starttls", "tls", "none":
	default:
		return fmt.Errorf("unknown security %q", e.Security)
	}
	if e.DailyHour < 0 || e.DailyHour > 23 {
		return errors.New("daily report hour must be 0-23")
	}
	for _, a := range append([]string{e.From}, e.To...) {
		if !strings.Contains(a, "@") || strings.ContainsAny(a, "\r\n<>,") {
			return fmt.Errorf("invalid email address %q", a)
		}
	}
	return nil
}

// sendMail delivers a plain text message.
// attachment is a file attached to an email.
type attachment struct {
	Name, ContentType string
	Data              []byte
}

func sendMail(ctx context.Context, e EmailSettings, subject, body string) error {
	return sendMailWith(ctx, e, subject, body, nil)
}

func sendMailWith(ctx context.Context, e EmailSettings, subject, body string, atts []attachment) error {
	addr := net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
	d := net.Dialer{Timeout: 20 * time.Second}
	var conn net.Conn
	var err error
	if e.Security == "tls" {
		conn, err = tls.DialWithDialer(&d, "tcp", addr, &tls.Config{ServerName: e.Host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("connect to %s: %w", addr, err)
	}
	conn.SetDeadline(time.Now().Add(60 * time.Second))
	c, err := smtp.NewClient(conn, e.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("smtp: %w", err)
	}
	defer c.Close()
	if e.Security == "starttls" {
		if err := c.StartTLS(&tls.Config{ServerName: e.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("starttls: %w", err)
		}
	}
	if e.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", e.Username, e.Password, e.Host)); err != nil {
			return fmt.Errorf("login: %w", err)
		}
	}
	if err := c.Mail(e.From); err != nil {
		return fmt.Errorf("sender rejected: %w", err)
	}
	for _, to := range e.To {
		if err := c.Rcpt(to); err != nil {
			return fmt.Errorf("recipient %s rejected: %w", to, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	var id [12]byte
	rand.Read(id[:])
	hdr := fmt.Sprintf("From: BackupZit <%s>\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s@backupzit>\r\nMIME-Version: 1.0\r\n",
		e.From, strings.Join(e.To, ", "), mime.QEncoding.Encode("utf-8", subject), time.Now().Format(time.RFC1123Z), hex.EncodeToString(id[:]))
	text := strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n")
	msg := hdr + "Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n" + text
	if len(atts) > 0 {
		boundary := "bz-" + hex.EncodeToString(id[:])
		var b strings.Builder
		b.WriteString(hdr + "Content-Type: multipart/mixed; boundary=\"" + boundary + "\"\r\n\r\n")
		b.WriteString("--" + boundary + "\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n" + text + "\r\n")
		clean := strings.NewReplacer("\"", "", "\r", "", "\n", "")
		for _, a := range atts {
			b.WriteString("--" + boundary + "\r\nContent-Type: " + clean.Replace(a.ContentType) + "\r\nContent-Transfer-Encoding: base64\r\n" +
				"Content-Disposition: attachment; filename=\"" + clean.Replace(a.Name) + "\"\r\n\r\n")
			enc := base64.StdEncoding.EncodeToString(a.Data)
			for len(enc) > 76 {
				b.WriteString(enc[:76] + "\r\n")
				enc = enc[76:]
			}
			b.WriteString(enc + "\r\n")
		}
		b.WriteString("--" + boundary + "--\r\n")
		msg = b.String()
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// Notifier sends run alerts and daily reports.
type Notifier struct {
	store     *Store
	log       *slog.Logger
	publicURL func() string
	mu        sync.Mutex // one pass at a time
	send      func(ctx context.Context, e EmailSettings, subject, body string) error
	sendWith  func(ctx context.Context, e EmailSettings, subject, body string, atts []attachment) error
}

func NewNotifier(store *Store, log *slog.Logger, publicURL func() string) *Notifier {
	return &Notifier{store: store, log: log, publicURL: publicURL, send: sendMail, sendWith: sendMailWith}
}

func wants(e EmailSettings, status string) bool {
	switch status {
	case api.StatusFailed:
		return e.OnFailure
	case api.StatusWarning:
		return e.OnWarning
	case api.StatusSuccess:
		return e.OnSuccess
	}
	return false
}

// Pass sends alerts for finished runs that have not been handled yet, and
// the daily report when it is due.
func (n *Notifier) Pass(ctx context.Context, now time.Time) {
	n.mu.Lock()
	defer n.mu.Unlock()
	var e EmailSettings
	if err := n.store.GetSetting(ctx, settingEmail, &e); err != nil {
		n.log.Error("load email settings", "err", err)
		return
	}
	n.runAlerts(ctx, e)
	if e.Enabled {
		n.scheduledReports(ctx, e, now)
	}
	if e.Enabled && e.DailyReport && now.Hour() == e.DailyHour {
		var last string
		n.store.GetSetting(ctx, settingDailySent, &last)
		today := now.Format("2006-01-02")
		if last != today {
			if err := n.dailyReport(ctx, e, now); err != nil {
				n.log.Error("daily report", "err", err)
			} else {
				n.store.SetSetting(ctx, settingDailySent, today)
			}
		}
	}
}

func (n *Notifier) runAlerts(ctx context.Context, e EmailSettings) {
	rows, err := n.store.db.Query(ctx, `SELECT id FROM runs WHERE NOT notified AND finished_at IS NOT NULL ORDER BY finished_at LIMIT 50`)
	if err != nil {
		n.log.Error("pending notifications", "err", err)
		return
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return
	}
	for _, id := range ids {
		run, err := n.store.GetRun(ctx, id)
		if err != nil {
			continue
		}
		if e.Enabled && (wants(e, run.Status) || (run.Anomaly != "" && e.OnFailure)) {
			subject, body := n.runMessage(run)
			if err := n.send(ctx, e, subject, body); err != nil {
				// Leave unhandled to retry on the next pass; stop to avoid
				// hammering a broken mail server.
				n.log.Warn("send run notification", "run", id, "err", err)
				return
			}
			n.log.Info("notification sent", "run", id, "status", run.Status)
		}
		n.store.db.Exec(ctx, `UPDATE runs SET notified=true WHERE id=$1`, id)
	}
}

func kindName(k string) string {
	return map[string]string{
		api.KindBackup: "Backup", api.KindRestore: "Restore", api.KindImageBackup: "Image backup",
		api.KindImageRestore: "Image restore", api.KindImageFileRestore: "File restore from image", api.KindVMBackup: "VM backup", api.KindVMRestore: "VM restore",
		api.KindCopy: "Backup copy", api.KindVerify: "Restore test",
	}[k]
}

func (n *Notifier) runMessage(r Run) (string, string) {
	job := ""
	if r.JobName != nil {
		job = " \"" + *r.JobName + "\""
	}
	subject := fmt.Sprintf("[BackupZit] %s %s%s on %s", strings.ToUpper(r.Status), kindName(r.Kind), job, r.Hostname)
	var b strings.Builder
	if r.Anomaly != "" {
		subject = fmt.Sprintf("[BackupZit] SUSPICIOUS %s%s on %s — possible ransomware or mass deletion", kindName(r.Kind), job, r.Hostname)
		fmt.Fprintf(&b, "WARNING: this backup looks unusual: %s.\n\nCheck the machine for ransomware or accidental mass deletion. Retention of this job is paused, so older backups are kept until you review it in the console.\n\n", r.Anomaly)
	}
	fmt.Fprintf(&b, "%s%s on %s finished with status %s.\n\n", kindName(r.Kind), job, r.Hostname, strings.ToUpper(r.Status))
	if r.StartedAt != nil {
		fmt.Fprintf(&b, "Started:  %s\n", r.StartedAt.Local().Format("2006-01-02 15:04:05"))
	}
	if r.FinishedAt != nil {
		fmt.Fprintf(&b, "Finished: %s (%s)\n", r.FinishedAt.Local().Format("2006-01-02 15:04:05"), r.Duration())
	}
	if r.Message != "" {
		fmt.Fprintf(&b, "Message:  %s\n", r.Message)
	}
	if len(r.Errors) > 0 {
		fmt.Fprintf(&b, "\n%d errors:\n", len(r.Errors))
		for i, e := range r.Errors {
			if i == 20 {
				fmt.Fprintf(&b, "  ... and %d more\n", len(r.Errors)-20)
				break
			}
			fmt.Fprintf(&b, "  %s\n", e)
		}
	}
	if u := n.publicURL(); u != "" {
		fmt.Fprintf(&b, "\nDetails: %s/runs/%d\n", u, r.ID)
	}
	return subject, b.String()
}

func (n *Notifier) dailyReport(ctx context.Context, e EmailSettings, now time.Time) error {
	runs, err := n.store.ListRuns(ctx, RunFilter{Limit: 1000})
	if err != nil {
		return err
	}
	since := now.Add(-24 * time.Hour)
	count := map[string]int{}
	var problems []Run
	for _, r := range runs {
		if r.QueuedAt.Before(since) {
			continue
		}
		count[r.Status]++
		if r.Status == api.StatusFailed || r.Status == api.StatusWarning {
			problems = append(problems, r)
		}
	}
	agents, err := n.store.ListAgents(ctx)
	if err != nil {
		return err
	}
	var offline []string
	for _, a := range agents {
		if !a.Online() && !a.Recovery {
			offline = append(offline, a.Hostname)
		}
	}
	state := "OK"
	if count[api.StatusFailed] > 0 || len(offline) > 0 {
		state = "ATTENTION"
	} else if count[api.StatusWarning] > 0 {
		state = "WARNINGS"
	}
	subject := fmt.Sprintf("[BackupZit] Daily report %s: %s", now.Format("2006-01-02"), state)
	var b strings.Builder
	fmt.Fprintf(&b, "Last 24 hours: %d successful, %d with warnings, %d failed, %d queued/running.\n",
		count[api.StatusSuccess], count[api.StatusWarning], count[api.StatusFailed], count[api.StatusQueued]+count[api.StatusRunning])
	if len(problems) > 0 {
		b.WriteString("\nProblems:\n")
		for _, r := range problems {
			job := ""
			if r.JobName != nil {
				job = " \"" + *r.JobName + "\""
			}
			fmt.Fprintf(&b, "  %-7s %s%s on %s: %s\n", strings.ToUpper(r.Status), kindName(r.Kind), job, r.Hostname, r.Message)
		}
	}
	if len(offline) > 0 {
		fmt.Fprintf(&b, "\nAgents not seen in the last minutes: %s\n", strings.Join(offline, ", "))
	}
	if u := n.publicURL(); u != "" {
		fmt.Fprintf(&b, "\nConsole: %s\n", u)
	}
	return n.send(ctx, e, subject, b.String())
}
