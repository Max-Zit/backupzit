package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Recovery codes are enrollment tokens with a longer lifetime, meant to be
// stored on recovery media.
const recoveryTokenTTL = 30 * 24 * time.Hour

func (s *Server) recoveryPage(w http.ResponseWriter, r *http.Request, user string, token string, exp time.Time) {
	var isos []download
	for _, d := range s.downloads() {
		if strings.HasSuffix(strings.ToLower(d.Name), ".iso") {
			isos = append(isos, d)
		}
	}
	agents, err := s.store.ListAgents(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	var rec []Agent
	for _, a := range agents {
		if a.Recovery {
			rec = append(rec, a)
		}
	}
	s.render(w, r, "recovery", pageData{Title: "Recovery", Nav: "recovery", User: user, Data: map[string]any{
		"ISOs": isos, "Token": token, "Expires": exp, "ServerURL": s.publicURL(r),
		"Fingerprint": s.CertFingerprint, "Agents": rec,
	}})
}

func (s *Server) publicURL(r *http.Request) string {
	if s.PublicURL != "" {
		return s.PublicURL
	}
	return "https://" + r.Host
}

func (s *Server) handleRecovery(w http.ResponseWriter, r *http.Request, user string) {
	s.recoveryPage(w, r, user, "", time.Time{})
}

func (s *Server) handleRecoveryToken(w http.ResponseWriter, r *http.Request, user string) {
	tok, exp, err := s.store.CreateEnrollmentToken(r.Context(), recoveryTokenTTL)
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.recoveryPage(w, r, user, tok, exp)
}

// handleRecoveryJSON returns recovery.json for the token posted in the
// form (tokens are never put into URLs).
func (s *Server) handleRecoveryJSON(w http.ResponseWriter, r *http.Request, _ string) {
	tok := strings.TrimSpace(r.FormValue("token"))
	if tok == "" {
		http.Error(w, "missing token", http.StatusBadRequest)
		return
	}
	b, _ := json.MarshalIndent(map[string]string{
		"server": s.publicURL(r), "token": tok, "fingerprint": s.CertFingerprint,
	}, "", "  ")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="recovery.json"`)
	w.Write(b)
}
