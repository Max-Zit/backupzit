package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// confirmIdentity makes the signed-in user prove again who they are before
// backups are deleted, so that a stolen browser session alone cannot
// destroy them: the current two-factor code (or a recovery code) when
// two-factor sign-in is on, otherwise the password. Failed attempts count
// like failed sign-ins.
func (s *Server) confirmIdentity(r *http.Request) error {
	me := currentUser(r)
	if me == nil {
		return errors.New("sign in again to delete backups")
	}
	if wait := s.guard.Blocked(r, me.Username); wait > 0 {
		return fmt.Errorf("Too many failed sign-ins. Try again in %d minutes.", int(wait.Minutes())+1)
	}
	if me.TwoFactor {
		if _, err := s.store.checkSecondFactor(r.Context(), me.ID, strings.TrimSpace(r.FormValue("reauth_code")), s.clock()); err != nil {
			s.guard.Fail(r, me.Username, "confirmations")
			return errors.New("To delete backups, enter the current code from your authenticator app.")
		}
		s.guard.Success(r, me.Username)
		return nil
	}
	if me.Source == "sso" {
		return errors.New("To delete backups, turn on two-factor sign-in under My account first.")
	}
	if _, err := s.authenticate(r.Context(), me.Username, r.FormValue("reauth_password")); err != nil {
		s.guard.Fail(r, me.Username, "confirmations")
		return errors.New("To delete backups, enter your password.")
	}
	s.guard.Success(r, me.Username)
	return nil
}
