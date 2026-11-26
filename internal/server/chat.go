package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/max-zit/backupzit/internal/api"
)

// Chat and webhook notifications: besides email, run results go to Slack,
// Microsoft Teams or any HTTP endpoint (generic JSON webhook).

const settingChat = "chat_channels"

// ChatChannel is one Slack, Teams or webhook destination.
type ChatChannel struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"` // slack | teams | webhook
	URL       string `json:"url"`
	Secret    string `json:"secret,omitempty"` // webhook: HMAC-SHA256 signing key
	OnFailure bool   `json:"on_failure"`
	OnWarning bool   `json:"on_warning"`
	OnSuccess bool   `json:"on_success"`
}

// ChatSettings are all chat and webhook channels.
type ChatSettings struct {
	Channels []ChatChannel `json:"channels"`
}

func (c *ChatChannel) Validate() error {
	c.Name, c.URL = strings.TrimSpace(c.Name), strings.TrimSpace(c.URL)
	if c.Name == "" || len(c.Name) > 100 {
		return errors.New("give the channel a name")
	}
	switch c.Kind {
	case "slack", "teams", "webhook":
	default:
		return errors.New("choose Slack, Microsoft Teams or webhook")
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && c.Kind == "webhook")) {
		return errors.New("enter the webhook URL (https://…)")
	}
	if c.Kind != "webhook" {
		c.Secret = ""
	}
	return nil
}

// wantsStatus reports whether the channel wants runs with this status.
func (c ChatChannel) wantsStatus(status string, anomaly bool) bool {
	switch {
	case anomaly, status == api.StatusFailed:
		return c.OnFailure
	case status == api.StatusWarning:
		return c.OnWarning
	case status == api.StatusSuccess:
		return c.OnSuccess
	}
	return false
}

func (s *Store) chatSettings(ctx context.Context) ChatSettings {
	var c ChatSettings
	_ = s.GetSetting(ctx, settingChat, &c)
	return c
}

// webhookEvent is the JSON body of generic webhooks.
type webhookEvent struct {
	Event      string     `json:"event"` // "run.finished"
	RunID      int64      `json:"run_id"`
	Kind       string     `json:"kind"`
	KindTitle  string     `json:"kind_title"`
	Status     string     `json:"status"`
	Suspicious string     `json:"suspicious,omitempty"`
	Job        string     `json:"job,omitempty"`
	JobID      *int64     `json:"job_id,omitempty"`
	Agent      string     `json:"agent"`
	AgentID    int64      `json:"agent_id"`
	Message    string     `json:"message,omitempty"`
	Errors     []string   `json:"errors,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// How long the run took and, for backups, the files and bytes of the
	// source, the bytes read from it and the bytes uploaded to the storage.
	DurationS   int    `json:"duration_s"`
	Files       uint64 `json:"files,omitempty"`
	Bytes       uint64 `json:"bytes,omitempty"`
	BytesRead   uint64 `json:"bytes_read,omitempty"`
	BytesStored uint64 `json:"bytes_stored,omitempty"`
	Snapshot    string `json:"snapshot,omitempty"`
	URL         string `json:"url,omitempty"`
}

// chatClient posts to Slack, Teams and webhooks (A18): it follows no
// redirects (a webhook answers itself; a redirect could lead to an address
// the settings never showed) and connects to no link-local address (cloud
// metadata services at 169.254.169.254, fe80::/10). Private and loopback
// addresses stay allowed: internal webhooks are common.
var chatClient = &http.Client{
	Timeout:       20 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, Control: refuseLinkLocal}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		ForceAttemptHTTP2:   true,
	},
}

// refuseLinkLocal refuses connections to link-local addresses, checked on
// the address actually dialled (after DNS), so a name cannot point there.
func refuseLinkLocal(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if ip, err := netip.ParseAddr(host); err == nil && (ip.Unmap().IsLinkLocalUnicast() || ip.Unmap().IsLinkLocalMulticast()) {
		return fmt.Errorf("%s is a link-local address; webhooks cannot be sent there", host)
	}
	return nil
}

// sendChat posts a run result to a channel.
func (n *Notifier) sendChat(ctx context.Context, l *language, c ChatChannel, r Run) error {
	link := ""
	if u := n.publicURL(); u != "" {
		link = fmt.Sprintf("%s/runs/%d", u, r.ID)
	}
	title, _ := n.runMessage(l, r)
	title = strings.TrimPrefix(title, "[BackupZit] ")
	var body []byte
	switch c.Kind {
	case "slack":
		icon := map[string]string{api.StatusFailed: ":red_circle:", api.StatusWarning: ":large_yellow_circle:", api.StatusSuccess: ":large_green_circle:"}[r.Status]
		if r.Anomaly != "" {
			icon = ":rotating_light:"
		}
		text := fmt.Sprintf("%s *%s*", icon, slackEscape(title))
		if r.Message != "" {
			text += "\n" + slackEscape(r.Message)
		}
		if r.Anomaly != "" {
			text += "\n*" + slackEscape(l.T("Unusual backup:")) + "* " + slackEscape(l.T("%s — retention of the job is paused.", r.Anomaly))
		}
		if link != "" {
			text += fmt.Sprintf("\n<%s|%s>", link, slackEscape(l.T("Open in BackupZit")))
		}
		body, _ = json.Marshal(map[string]any{"text": text})
	case "teams":
		color := map[string]string{api.StatusFailed: "Attention", api.StatusWarning: "Warning", api.StatusSuccess: "Good"}[r.Status]
		items := []any{teamsText(title, true, color)}
		if r.Message != "" {
			items = append(items, teamsText(r.Message, false, ""))
		}
		if r.Anomaly != "" {
			items = append(items, teamsText(l.T("Unusual backup:")+" "+l.T("%s — retention of the job is paused.", r.Anomaly), false, "Attention"))
		}
		card := map[string]any{"type": "AdaptiveCard", "$schema": "http://adaptivecards.io/schemas/adaptive-card.json", "version": "1.4", "body": items}
		if link != "" {
			card["actions"] = []any{map[string]any{"type": "Action.OpenUrl", "title": l.T("Open in BackupZit"), "url": link}}
		}
		body, _ = json.Marshal(map[string]any{"type": "message", "attachments": []any{map[string]any{"contentType": "application/vnd.microsoft.card.adaptive", "content": card}}})
	default:
		ev := webhookEvent{Event: "run.finished", RunID: r.ID, Kind: r.Kind, KindTitle: kindName(r.Kind), Status: r.Status, Suspicious: r.Anomaly,
			JobID: r.JobID, Agent: r.Hostname, Message: r.Message, Errors: r.Errors, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, URL: link}
		if r.JobName != nil {
			ev.Job = *r.JobName
		}
		ev.AgentID, ev.DurationS, ev.Snapshot = r.AgentID, int(r.Duration().Seconds()), r.SnapshotID
		var st runStats
		if len(r.Stats) > 0 && json.Unmarshal(r.Stats, &st) == nil {
			ev.Files, ev.Bytes, ev.BytesRead, ev.BytesStored = st.Files, st.Bytes, st.BytesRead, st.BytesStored
		}
		body, _ = json.Marshal(ev)
	}
	return postChat(ctx, c, body)
}

// sendChatText posts a message that is not about one run (e.g. a missed
// RPO): a title, a line of text and a console path to link; webhooks get
// event as JSON (with "url").
func (n *Notifier) sendChatText(ctx context.Context, l *language, c ChatChannel, title, text, path string, event map[string]any) error {
	link := ""
	if u := n.publicURL(); u != "" {
		link = u + path
	}
	var body []byte
	switch c.Kind {
	case "slack":
		msg := ":warning: *" + slackEscape(title) + "*\n" + slackEscape(text)
		if link != "" {
			msg += fmt.Sprintf("\n<%s|%s>", link, slackEscape(l.T("Open in BackupZit")))
		}
		body, _ = json.Marshal(map[string]any{"text": msg})
	case "teams":
		card := map[string]any{"type": "AdaptiveCard", "$schema": "http://adaptivecards.io/schemas/adaptive-card.json", "version": "1.4",
			"body": []any{teamsText(title, true, "Attention"), teamsText(text, false, "")}}
		if link != "" {
			card["actions"] = []any{map[string]any{"type": "Action.OpenUrl", "title": l.T("Open in BackupZit"), "url": link}}
		}
		body, _ = json.Marshal(map[string]any{"type": "message", "attachments": []any{map[string]any{"contentType": "application/vnd.microsoft.card.adaptive", "content": card}}})
	default:
		ev := map[string]any{}
		for k, v := range event {
			ev[k] = v
		}
		if link != "" {
			ev["url"] = link
		}
		body, _ = json.Marshal(ev)
	}
	return postChat(ctx, c, body)
}

func postChat(ctx context.Context, c ChatChannel, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "BackupZit")
	if c.Secret != "" {
		m := hmac.New(sha256.New, []byte(c.Secret))
		m.Write(body)
		req.Header.Set("X-BackupZit-Signature", "sha256="+hex.EncodeToString(m.Sum(nil)))
	}
	resp, err := chatClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("%s answered %s: %s", c.Name, resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// ---- settings page

func (s *Server) handleSettingsChat(w http.ResponseWriter, r *http.Request, _ string) {
	ctx := r.Context()
	cs := s.store.chatSettings(ctx)
	back := "/settings/chat"
	switch r.FormValue("action") {
	case "add":
		c := ChatChannel{Name: r.FormValue("name"), Kind: r.FormValue("kind"), URL: r.FormValue("url"), Secret: strings.TrimSpace(r.FormValue("secret")),
			OnFailure: r.FormValue("on_failure") == "on", OnWarning: r.FormValue("on_warning") == "on", OnSuccess: r.FormValue("on_success") == "on"}
		if err := c.Validate(); err != nil {
			redirectErr(w, r, back, err)
			return
		}
		b := make([]byte, 6)
		rand.Read(b)
		c.ID = hex.EncodeToString(b)
		cs.Channels = append(cs.Channels, c)
		s.audit(r, "settings.chat", "added %s channel %q", c.Kind, c.Name)
	case "delete", "test", "update":
		i := -1
		for k, c := range cs.Channels {
			if c.ID == r.FormValue("id") {
				i = k
			}
		}
		if i < 0 {
			redirectErr(w, r, back, errors.New("unknown channel"))
			return
		}
		c := cs.Channels[i]
		switch r.FormValue("action") {
		case "delete":
			cs.Channels = append(cs.Channels[:i], cs.Channels[i+1:]...)
			s.audit(r, "settings.chat", "removed channel %q", c.Name)
		case "update":
			c.OnFailure, c.OnWarning, c.OnSuccess = r.FormValue("on_failure") == "on", r.FormValue("on_warning") == "on", r.FormValue("on_success") == "on"
			cs.Channels[i] = c
			s.audit(r, "settings.chat", "changed channel %q", c.Name)
		case "test":
			n := s.Notifier
			if n == nil {
				n = NewNotifier(s.store, s.log, s.ConsoleURL)
			}
			var es EmailSettings
			_ = s.store.GetSetting(ctx, settingEmail, &es)
			l := notifyLanguage(es)
			now := time.Now()
			name := "Test"
			test := Run{ID: 0, Kind: api.KindBackup, Status: api.StatusSuccess, Hostname: "backupzit", JobName: &name, StartedAt: &now, FinishedAt: &now,
				Message: l.T("This is a test message from BackupZit: the channel is configured correctly.")}
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if err := n.sendChat(ctx, l, c, test); err != nil {
				redirectErr(w, r, back, fmt.Errorf("test message failed: %w", err))
				return
			}
			redirectMsg(w, r, back, "Test message sent to "+c.Name+".")
			return
		}
	default:
		redirectErr(w, r, back, errors.New("unknown action"))
		return
	}
	if err := s.store.SetSetting(ctx, settingChat, cs); err != nil {
		s.serverError(w, err)
		return
	}
	redirectMsg(w, r, back, "Settings saved.")
}

// teamsText is a block of plain text for a Teams card (B11, 0.62): a
// RichTextBlock with one TextRun, which Teams shows as it is, while a
// TextBlock reads markdown, so a job name or a message with [text](link)
// became a link in the channel.
func teamsText(text string, bold bool, color string) map[string]any {
	run := map[string]any{"type": "TextRun", "text": text}
	if bold {
		run["weight"] = "Bolder"
	}
	if color != "" {
		run["color"] = color
	}
	return map[string]any{"type": "RichTextBlock", "inlines": []any{run}}
}
