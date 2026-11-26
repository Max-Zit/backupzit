package server

import (
	"context"
	"errors"
	"sync"
	"time"
)

// API tokens of LDAP users are checked against the directory (B8, 0.62):
// a token is not a sign-in, so before, a user removed from the directory
// or from every BackupZit group kept the token's rights until it expired.
// The answer is cached; while the directory cannot be reached, the last
// answer counts for a day.

const (
	ldapTokenCheckTTL = 15 * time.Minute
	ldapTokenGrace    = 24 * time.Hour
)

type ldapTokenState struct {
	role    string // role from the directory; "" = no account or no group
	checked time.Time
}

type ldapTokenCache struct {
	mu sync.Mutex
	m  map[string]ldapTokenState
}

// errTokenDirectory: the token's directory account is gone or has no role.
var errTokenDirectory = errors.New("the directory account of this token no longer exists or is in no BackupZit group")

// ldapLookup checks a directory account without its password (tests
// replace it).
var ldapLookup = func(ctx context.Context, l LDAPSettings, username string) (role string, err error) {
	conn, err := l.connect(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	res, err := l.lookup(conn, username)
	if err != nil {
		return "", err
	}
	if err := l.resolveRole(conn, username, res); err != nil {
		return "", err
	}
	return res.Role, nil
}

// checkLDAPToken returns the role the directory gives the user of an API
// token now, or errTokenDirectory.
func (s *Server) checkLDAPToken(ctx context.Context, username string) (string, error) {
	s.ldapTokens.mu.Lock()
	if s.ldapTokens.m == nil {
		s.ldapTokens.m = map[string]ldapTokenState{}
	}
	st, ok := s.ldapTokens.m[username]
	s.ldapTokens.mu.Unlock()
	now := time.Now()
	if ok && now.Sub(st.checked) < ldapTokenCheckTTL {
		if st.role == "" {
			return "", errTokenDirectory
		}
		return st.role, nil
	}
	var l LDAPSettings
	if err := s.store.GetSetting(ctx, settingLDAP, &l); err != nil || !l.Enabled {
		return "", errTokenDirectory // LDAP sign-in was turned off
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	role, err := ldapLookup(cctx, l, username)
	switch {
	case err == nil:
	case errors.Is(err, errBadLogin), errors.Is(err, errNoRole):
		role = ""
	default:
		// The directory cannot be reached: the last answer counts a while.
		s.log.Warn("API token: directory check failed", "user", username, "err", err)
		if ok && now.Sub(st.checked) < ldapTokenGrace && st.role != "" {
			return st.role, nil
		}
		return "", errTokenDirectory
	}
	s.ldapTokens.mu.Lock()
	s.ldapTokens.m[username] = ldapTokenState{role: role, checked: now}
	s.ldapTokens.mu.Unlock()
	if role == "" {
		return "", errTokenDirectory
	}
	return role, nil
}
