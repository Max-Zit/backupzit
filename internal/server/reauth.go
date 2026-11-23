package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
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

// confirmAdmin makes the signed-in user prove who they are before they
// create a user or set another user's password: otherwise a stolen session
// creates an account with a known password (or takes over another one),
// signs in with it and passes every confirmIdentity there (H-4). The
// current two-factor code when that is on, single sign-on users a sign-in
// within ssoFreshSignIn, everyone else the password.
func (s *Server) confirmAdmin(r *http.Request) error {
	me := currentUser(r)
	if me == nil {
		return errors.New("sign in again")
	}
	if wait := s.guard.Blocked(r, me.Username); wait > 0 {
		return fmt.Errorf("Too many failed sign-ins. Try again in %d minutes.", int(wait.Minutes())+1)
	}
	switch {
	case me.TwoFactor:
		code := strings.TrimSpace(r.FormValue("reauth_code"))
		if code == "" {
			return errors.New("Enter the current code from your authenticator app to confirm.")
		}
		if _, err := s.store.checkSecondFactor(r.Context(), me.ID, code, s.clock()); err != nil {
			s.guard.Fail(r, me.Username, "confirmations")
			return errors.New("Enter the current code from your authenticator app to confirm.")
		}
	case me.Source == "sso":
		if age, ok := s.sessionAge(r); !ok || age > ssoFreshSignIn {
			return errors.New("Sign in again through single sign-on, then do this within 10 minutes.")
		}
		return nil
	default:
		pw := r.FormValue("reauth_password")
		if pw == "" { // not an attempt: no failed sign-in
			return errors.New("Enter your password to confirm.")
		}
		if _, err := s.authenticate(r.Context(), me.Username, pw); err != nil {
			s.guard.Fail(r, me.Username, "confirmations")
			return errors.New("Enter your password to confirm.")
		}
	}
	s.guard.Success(r, me.Username)
	return nil
}

// ssoFreshSignIn is how recent a single sign-on must be before such a user
// turns on two-factor sign-in (they have no password to confirm).
const ssoFreshSignIn = 10 * time.Minute

// confirmEnrollment makes the signed-in user prove who they are before
// two-factor sign-in is turned on: otherwise a stolen session could enroll
// the thief's authenticator and then pass confirmIdentity with its codes.
// Local and LDAP users enter their password; single sign-on users must have
// signed in within ssoFreshSignIn.
func (s *Server) confirmEnrollment(r *http.Request) error {
	me := currentUser(r)
	if me == nil {
		return errors.New("sign in again")
	}
	if wait := s.guard.Blocked(r, me.Username); wait > 0 {
		return fmt.Errorf("Too many failed sign-ins. Try again in %d minutes.", int(wait.Minutes())+1)
	}
	if me.Source == "sso" {
		if age, ok := s.sessionAge(r); !ok || age > ssoFreshSignIn {
			return errors.New("Sign in again through single sign-on, then turn on two-factor sign-in within 10 minutes.")
		}
		return nil
	}
	if _, err := s.authenticate(r.Context(), me.Username, r.FormValue("password")); err != nil {
		s.guard.Fail(r, me.Username, "confirmations")
		return errors.New("Enter your current password to turn on two-factor sign-in.")
	}
	s.guard.Success(r, me.Username)
	return nil
}

// checkOwnCode checks a current two-factor code (or a recovery code) of the
// signed-in user on the account pages, counted like failed sign-ins.
func (s *Server) checkOwnCode(r *http.Request, msg string) error {
	me := currentUser(r)
	if me == nil {
		return errors.New("sign in again")
	}
	if wait := s.guard.Blocked(r, me.Username); wait > 0 {
		return fmt.Errorf("Too many failed sign-ins. Try again in %d minutes.", int(wait.Minutes())+1)
	}
	if _, err := s.store.checkSecondFactor(r.Context(), me.ID, strings.TrimSpace(r.FormValue("code")), s.clock()); err != nil {
		s.guard.Fail(r, me.Username, "confirmations")
		return errors.New(msg)
	}
	s.guard.Success(r, me.Username)
	return nil
}

// sessionAge is how long ago the request's session was created.
func (s *Server) sessionAge(r *http.Request) (time.Duration, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return 0, false
	}
	var secs float64
	if err := s.store.db.QueryRow(r.Context(), `SELECT EXTRACT(EPOCH FROM now()-created_at)::float8 FROM sessions WHERE token_hash=$1`,
		hashToken(c.Value)).Scan(&secs); err != nil {
		return 0, false
	}
	return time.Duration(secs * float64(time.Second)), true
}

// The users page does not change the signed-in user's own password or
// two-factor sign-in: My account asks for the current password or code.
var (
	errOwnPassword = errors.New("Change your own password under My account; it asks for the current one.")
	errOwn2FAReset = errors.New("Turn off your own two-factor sign-in under My account; it asks for a current code. If you lost it, another administrator can reset it.")
)

// revokeTokens deletes the API tokens of a user (after a password reset by
// someone else: whoever knew the old password may have created them).
func (s *Store) revokeTokens(ctx context.Context, userID int64) (int64, error) {
	tag, err := s.db.Exec(ctx, `DELETE FROM api_tokens WHERE user_id=$1`, userID)
	return tag.RowsAffected(), err
}
