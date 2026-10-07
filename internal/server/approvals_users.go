package server

import (
	"context"
	"encoding/json"
	"errors"

	"strings"

	"golang.org/x/crypto/bcrypt"
)

// With four-eyes approval on, a single administrator must not be able to
// get a second approver on their own: creating a user who can approve,
// giving a user such a role (or enabling one), resetting the password or
// two-factor sign-in of such a user and changing LDAP or single sign-on
// (which decide the roles of directory users) all wait for approval.
// Approvers must also have existed before the request, so an account
// created afterwards cannot decide it. Passwords travel in requests only
// as hashes, LDAP and SSO settings sealed with the secrets key.

// approvalPerms are the permissions that let a user decide requests.
var approvalPerms = []Perm{PermJobs, PermAgents, PermStorage, PermSettings, PermUsers}

// canApproveRole reports whether a role can decide some requests.
func canApproveRole(role string) bool {
	r, ok := roleByKey(role)
	if !ok {
		return false
	}
	for _, p := range approvalPerms {
		if r.Has(p) {
			return true
		}
	}
	return false
}

// gainsApproval reports whether changing a user from (oldRole, oldOff) to
// (newRole, newOff) lets it decide requests it could not decide before.
func gainsApproval(oldRole string, oldOff bool, newRole string, newOff bool) bool {
	if newOff {
		return false
	}
	nr, ok := roleByKey(newRole)
	if !ok {
		return false
	}
	or, _ := roleByKey(oldRole)
	for _, p := range approvalPerms {
		if nr.Has(p) && (oldOff || !or.Has(p)) {
			return true
		}
	}
	return false
}

// enabledAdmins counts enabled administrators other than id.
func (s *Store) enabledAdmins(ctx context.Context, except int64) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM users WHERE role='admin' AND NOT disabled AND id<>$1`, except).Scan(&n)
	return n, err
}

// errTwoAdmins: four-eyes approval keeps two administrators to decide.
var errTwoAdmins = errors.New("four-eyes approval needs at least two enabled administrators; add another administrator first, or turn four-eyes approval off")

// keepsTwoAdmins refuses, with four-eyes approval on, a change that leaves
// fewer than two enabled administrators (no one could approve then).
func (s *Store) keepsTwoAdmins(ctx context.Context, changed User) error {
	if !s.fourEyes(ctx) || (changed.Role == "admin" && !changed.Disabled) {
		return nil
	}
	n, err := s.enabledAdmins(ctx, changed.ID)
	if err != nil {
		return err
	}
	if n < 2 {
		return errTwoAdmins
	}
	return nil
}

// newUserRequest is the payload of "user.create".
type newUserRequest struct {
	User User   `json:"user"`
	Hash string `json:"hash"`
}

// hashNewPassword checks and hashes a password for a request.
func hashNewPassword(password string) (string, error) {
	if err := checkPassword(password); err != nil {
		return "", err
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(h), err
}

// checkNewUser validates a user to create, before it is requested.
func (s *Store) checkNewUser(ctx context.Context, u *User, password string) (string, error) {
	u.Username = strings.TrimSpace(u.Username)
	if !validUsername.MatchString(u.Username) {
		return "", errors.New("username may contain letters, digits, '.', '_', '-' and '@' (max. 64)")
	}
	if _, ok := roleByKey(u.Role); !ok {
		return "", errors.New("choose a role")
	}
	var exists bool
	s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE lower(username)=lower($1))`, u.Username).Scan(&exists)
	if exists {
		return "", errors.New("a user with that name already exists")
	}
	return hashNewPassword(password)
}

// createUserHashed creates a local user with a hashed password.
func (s *Store) createUserHashed(ctx context.Context, u User, hash string) (int64, error) {
	var id int64
	err := s.db.QueryRow(ctx, `INSERT INTO users(username, password_hash, display_name, email, role, source)
		VALUES($1,$2,$3,$4,$5,'local') RETURNING id`, u.Username, hash, strings.TrimSpace(u.DisplayName), strings.TrimSpace(u.Email), u.Role).Scan(&id)
	if err != nil && strings.Contains(err.Error(), "duplicate key") {
		return 0, errors.New("a user with that name already exists")
	}
	return id, err
}

// setUserPasswordHash sets a local user's password hash and signs it out.
func (s *Store) setUserPasswordHash(ctx context.Context, id int64, hash string) error {
	tag, err := s.db.Exec(ctx, `UPDATE users SET password_hash=$2 WHERE id=$1 AND source='local'`, id, hash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("only local accounts have a password here; LDAP passwords are changed in the directory")
	}
	_, err = s.db.Exec(ctx, `DELETE FROM sessions WHERE user_id=$1`, id)
	return err
}

const ctxApprovalPayload = "approvals.payload"

// sealedPayload wraps settings with secrets for a request.
type sealedPayload struct {
	Sealed string `json:"sealed"`
}

func (s *Store) sealPayload(v any) (sealedPayload, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return sealedPayload{}, err
	}
	return sealedPayload{Sealed: s.seal(ctxApprovalPayload, string(b))}, nil
}

func (s *Store) openPayload(raw json.RawMessage, v any) error {
	var p sealedPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	plain := p.Sealed
	if s.box != nil {
		var err error
		if plain, err = s.box.open(ctxApprovalPayload, p.Sealed); err != nil {
			return err
		}
	}
	return json.Unmarshal([]byte(plain), v)
}

// executeUserApproval carries out approved user and sign-in changes; ok is
// false for other actions.
func (s *Server) executeUserApproval(ctx context.Context, a Approval) (ok bool, err error) {
	switch a.Action {
	case "user.create":
		var p newUserRequest
		if err := json.Unmarshal(a.Payload, &p); err != nil {
			return true, err
		}
		_, err := s.store.createUserHashed(ctx, p.User, p.Hash)
		return true, err
	case "user.role":
		var u User
		if err := json.Unmarshal(a.Payload, &u); err != nil {
			return true, err
		}
		cur, err := s.store.GetUser(ctx, a.ObjectID)
		if err != nil {
			return true, err
		}
		cur.DisplayName, cur.Email, cur.Role, cur.Disabled = u.DisplayName, u.Email, u.Role, u.Disabled
		return true, s.store.UpdateUser(ctx, cur)
	case "user.password":
		var hash string
		if err := json.Unmarshal(a.Payload, &hash); err != nil {
			return true, err
		}
		if err := s.store.setUserPasswordHash(ctx, a.ObjectID, hash); err != nil {
			return true, err
		}
		s.initialPasswordUsed(a.ObjectName)
		return true, nil
	case "user.2fa_reset":
		return true, s.store.DisableTwoFactor(ctx, a.ObjectID)
	case "settings.ldap":
		var l LDAPSettings
		if err := s.store.openPayload(a.Payload, &l); err != nil {
			return true, err
		}
		return true, s.store.SetSetting(ctx, settingLDAP, l)
	case "settings.sso":
		var c SSOSettings
		if err := s.store.openPayload(a.Payload, &c); err != nil {
			return true, err
		}
		return true, s.store.SetSetting(ctx, settingSSO, c)
	case "job.disable":
		return true, s.store.SetJobEnabled(ctx, a.ObjectID, false)
	}
	return false, nil
}

// approverStatus describes who can decide requests, for the settings page.
type approverStatus struct {
	Admins int
}

func (s *Server) approvers(ctx context.Context) approverStatus {
	n, _ := s.store.enabledAdmins(ctx, 0)
	return approverStatus{Admins: n}
}
