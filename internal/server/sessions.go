package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const settingSessions = "sessions"

// SessionSettings control how long a sign-in lasts.
type SessionSettings struct {
	// LifetimeHours is the maximum session length after signing in.
	LifetimeHours int `json:"lifetime_hours"`
	// IdleMinutes signs a user out after this much inactivity; 0 = never.
	IdleMinutes int `json:"idle_minutes"`
}

var defaultSessions = SessionSettings{LifetimeHours: 12, IdleMinutes: 60}

func (x SessionSettings) Validate() error {
	if x.LifetimeHours < 1 || x.LifetimeHours > 24*90 {
		return errors.New("session length must be between 1 hour and 90 days")
	}
	if x.IdleMinutes < 0 || x.IdleMinutes > 60*24*30 {
		return errors.New("inactivity timeout must be between 0 (off) and 30 days")
	}
	return nil
}

func (x SessionSettings) Lifetime() time.Duration { return time.Duration(x.LifetimeHours) * time.Hour }
func (x SessionSettings) Idle() time.Duration     { return time.Duration(x.IdleMinutes) * time.Minute }

// sessionCache avoids a settings query on every request.
type sessionCache struct {
	mu  sync.Mutex
	val SessionSettings
	at  time.Time
}

func (s *Server) sessionSettings(ctx context.Context) SessionSettings {
	c := &s.sessCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.at) < 30*time.Second {
		return c.val
	}
	x := defaultSessions
	if err := s.store.GetSetting(ctx, settingSessions, &x); err != nil || x.Validate() != nil {
		x = defaultSessions
	}
	c.val, c.at = x, time.Now()
	return x
}

// ApplySessionSettings shortens existing sessions to a new lifetime.
func (s *Store) ApplySessionSettings(ctx context.Context, x SessionSettings) error {
	if err := s.SetSetting(ctx, settingSessions, x); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `UPDATE sessions SET expires_at = LEAST(expires_at, created_at + make_interval(hours => $1))`, x.LifetimeHours)
	return err
}

func (s *Server) handleSettingsSessions(w http.ResponseWriter, r *http.Request, user string) {
	life, err1 := strconv.Atoi(r.FormValue("lifetime_hours"))
	idle, err2 := strconv.Atoi(r.FormValue("idle_minutes"))
	x := SessionSettings{LifetimeHours: life, IdleMinutes: idle}
	if err1 != nil || err2 != nil {
		redirectErr(w, r, "/settings", errors.New("enter whole numbers"))
		return
	}
	if err := x.Validate(); err != nil {
		redirectErr(w, r, "/settings", err)
		return
	}
	if err := s.store.ApplySessionSettings(r.Context(), x); err != nil {
		s.serverError(w, err)
		return
	}
	s.sessCache.mu.Lock()
	s.sessCache.at = time.Time{}
	s.sessCache.mu.Unlock()
	s.log.Info("session settings changed", "user", user, "lifetime_hours", x.LifetimeHours, "idle_minutes", x.IdleMinutes)
	s.audit(r, "settings.sessions", "lifetime %dh, idle %dmin", x.LifetimeHours, x.IdleMinutes)
	redirectMsg(w, r, "/settings", "Session settings saved. They apply to new sign-ins; existing sessions were shortened if needed.")
}
