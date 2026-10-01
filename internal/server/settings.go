package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request, user string) {
	var e EmailSettings
	if err := s.store.GetSetting(r.Context(), settingEmail, &e); err != nil {
		s.serverError(w, err)
		return
	}
	if e.Port == 0 {
		e.Port, e.Security, e.OnFailure, e.OnWarning, e.DailyHour = 587, "starttls", true, true, 8
	}
	s.render(w, r, "settings", pageData{Title: "Settings", Nav: "settings", User: user, Data: map[string]any{
		"Email": e, "HasPassword": e.Password != "", "Sessions": s.sessionSettings(r.Context()),
		"LDAP": ldapForPage(r.Context(), s.store), "ADFilter": adUserFilter, "LDAPFilter": ldapUserFilter,
	}})
}

// emailFromForm reads the email form; an empty password keeps the stored one.
func (s *Server) emailFromForm(r *http.Request) (EmailSettings, error) {
	var old EmailSettings
	if err := s.store.GetSetting(r.Context(), settingEmail, &old); err != nil {
		return old, err
	}
	port, _ := strconv.Atoi(r.FormValue("port"))
	hour, _ := strconv.Atoi(r.FormValue("daily_hour"))
	e := EmailSettings{
		Enabled:     r.FormValue("enabled") == "on",
		Host:        r.FormValue("host"),
		Port:        port,
		Security:    r.FormValue("security"),
		Username:    strings.TrimSpace(r.FormValue("username")),
		Password:    r.FormValue("password"),
		From:        r.FormValue("from"),
		To:          splitAddrs(r.FormValue("to")),
		OnFailure:   r.FormValue("on_failure") == "on",
		OnWarning:   r.FormValue("on_warning") == "on",
		OnSuccess:   r.FormValue("on_success") == "on",
		DailyReport: r.FormValue("daily_report") == "on",
		DailyHour:   hour,
	}
	if e.Password == "" && e.Username == old.Username {
		e.Password = old.Password
	}
	return e, e.Validate()
}

func splitAddrs(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == '\n' || r == ' ' }) {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func (s *Server) handleSettingsEmail(w http.ResponseWriter, r *http.Request, _ string) {
	e, err := s.emailFromForm(r)
	if err != nil {
		redirectErr(w, r, "/settings", err)
		return
	}
	if r.FormValue("action") == "test" {
		if !e.Enabled {
			e.Enabled = true
			if err := e.Validate(); err != nil {
				redirectErr(w, r, "/settings", err)
				return
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		body := "This is a test message from backupzit.\n\nEmail notifications are configured correctly."
		if err := sendMail(ctx, e, "[BackupZit] Test message", body); err != nil {
			redirectErr(w, r, "/settings", errors.New("test email failed: "+err.Error()))
			return
		}
		redirectMsg(w, r, "/settings", "Test email sent to "+strings.Join(e.To, ", ")+". Settings were not saved yet.")
		return
	}
	if err := s.store.SetSetting(r.Context(), settingEmail, e); err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, "settings.email", "email notifications enabled=%v", e.Enabled)
	redirectMsg(w, r, "/settings", "Settings saved.")
}
