package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"rsc.io/qr"
)

// ---- TOTP (RFC 6238): 30-second steps, 6 digits, HMAC-SHA1, as used by
// Google/Microsoft Authenticator, Authy, 1Password, Bitwarden …

const totpStep = 30

func newTOTPSecret() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
}

func totpCode(secret string, counter int64) (string, error) {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(secret))
	if err != nil {
		return "", err
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(counter))
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", v%1000000), nil
}

// totpVerify accepts the code of the current step or one step around it,
// but never a step at or before lastUsed (no replay). It returns the step.
func totpVerify(secret, code string, now time.Time, lastUsed int64) (int64, bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != 6 {
		return 0, false
	}
	cur := now.Unix() / totpStep
	for _, c := range []int64{cur, cur - 1, cur + 1} {
		if c <= lastUsed {
			continue
		}
		want, err := totpCode(secret, c)
		if err == nil && hmac.Equal([]byte(want), []byte(code)) {
			return c, true
		}
	}
	return 0, false
}

func totpURI(secret, account string) string {
	v := url.Values{"secret": {secret}, "issuer": {"BackupZit"}, "algorithm": {"SHA1"}, "digits": {"6"}, "period": {"30"}}
	return "otpauth://totp/" + url.PathEscape("BackupZit:"+account) + "?" + v.Encode()
}

// QR is a QR code as squares for an inline SVG (no images, CSP friendly).
type QR struct {
	Size  int
	Cells []struct{ X, Y int }
}

func qrFor(text string) (*QR, error) {
	c, err := qr.Encode(text, qr.M)
	if err != nil {
		return nil, err
	}
	q := &QR{Size: c.Size + 8}
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; x++ {
			if c.Black(x, y) {
				q.Cells = append(q.Cells, struct{ X, Y int }{x + 4, y + 4})
			}
		}
	}
	return q, nil
}

// ---- storage

// TwoFactor is a user's second factor state.
type TwoFactor struct {
	Enabled  bool
	Secret   string // decrypted
	LastStep int64
	Codes    []string // hashes of unused recovery codes
}

func (s *Store) getTwoFactor(ctx context.Context, userID int64) (TwoFactor, error) {
	var t TwoFactor
	var secret string
	err := s.db.QueryRow(ctx, `SELECT totp_enabled, totp_secret, totp_last_step, totp_recovery FROM users WHERE id=$1`, userID).
		Scan(&t.Enabled, &secret, &t.LastStep, &t.Codes)
	if err != nil {
		return t, notFound(err)
	}
	t.Secret, err = s.box.open("users.totp_secret", secret)
	return t, err
}

// setPendingSecret stores a new, not yet confirmed secret.
func (s *Store) setPendingSecret(ctx context.Context, userID int64, secret string) error {
	_, err := s.db.Exec(ctx, `UPDATE users SET totp_secret=$2, totp_enabled=false, totp_last_step=0 WHERE id=$1`,
		userID, s.box.seal("users.totp_secret", secret))
	return err
}

func hashCode(c string) string {
	c = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(c), "-", ""))
	h := sha256.Sum256([]byte("backupzit-recovery-code:" + c))
	return hex.EncodeToString(h[:])
}

func newRecoveryCodes() (plain, hashed []string) {
	for i := 0; i < 10; i++ {
		b := make([]byte, 5)
		rand.Read(b)
		c := strings.ToUpper(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)) // 8 chars
		plain = append(plain, c[:4]+"-"+c[4:])
		hashed = append(hashed, hashCode(c))
	}
	return
}

func (s *Store) enableTwoFactor(ctx context.Context, userID, step int64, codes []string) error {
	_, err := s.db.Exec(ctx, `UPDATE users SET totp_enabled=true, totp_last_step=$2, totp_recovery=$3 WHERE id=$1`, userID, step, codes)
	return err
}

func (s *Store) DisableTwoFactor(ctx context.Context, userID int64) error {
	_, err := s.db.Exec(ctx, `UPDATE users SET totp_enabled=false, totp_secret='', totp_last_step=0, totp_recovery='{}' WHERE id=$1`, userID)
	return err
}

// checkSecondFactor verifies a TOTP or recovery code and records its use.
func (s *Store) checkSecondFactor(ctx context.Context, userID int64, code string, now time.Time) (usedRecovery bool, err error) {
	t, err := s.getTwoFactor(ctx, userID)
	if err != nil {
		return false, err
	}
	if !t.Enabled {
		return false, errors.New("two-factor authentication is not enabled")
	}
	if step, ok := totpVerify(t.Secret, code, now, t.LastStep); ok {
		ct, err := s.db.Exec(ctx, `UPDATE users SET totp_last_step=$2 WHERE id=$1 AND totp_last_step < $2`, userID, step)
		if err != nil {
			return false, err
		}
		if ct.RowsAffected() == 0 {
			return false, errBadCode
		}
		return false, nil
	}
	h := hashCode(code)
	for _, c := range t.Codes {
		if hmac.Equal([]byte(c), []byte(h)) {
			ct, err := s.db.Exec(ctx, `UPDATE users SET totp_recovery=array_remove(totp_recovery, $2) WHERE id=$1 AND $2 = ANY(totp_recovery)`, userID, h)
			if err != nil {
				return false, err
			}
			if ct.RowsAffected() == 0 {
				return false, errBadCode
			}
			return true, nil
		}
	}
	return false, errBadCode
}

var errBadCode = errors.New("invalid code")

// ---- pending sign-in (password accepted, second factor missing)

const mfaCookie = "bz_mfa"

// mfaToken binds a user and an expiry, signed with the per-process key.
func mfaToken(userID int64, exp time.Time) string {
	payload := strconv.FormatInt(userID, 10) + "." + strconv.FormatInt(exp.Unix(), 10)
	m := hmac.New(sha256.New, flashKey)
	m.Write([]byte("mfa:" + payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func parseMFAToken(tok string, now time.Time) (int64, bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return 0, false
	}
	m := hmac.New(sha256.New, flashKey)
	m.Write([]byte("mfa:" + parts[0] + "." + parts[1]))
	if !hmac.Equal([]byte(parts[2]), []byte(base64.RawURLEncoding.EncodeToString(m.Sum(nil)))) {
		return 0, false
	}
	id, err1 := strconv.ParseInt(parts[0], 10, 64)
	exp, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil || now.Unix() > exp {
		return 0, false
	}
	return id, true
}

// ---- policy

// TwoFactorRequired says who must use a second factor: "", "admins", "all".
func (x SessionSettings) mustUse2FA(u *User) bool {
	switch x.Require2FA {
	case "all":
		return true
	case "admins":
		return u.Role == "admin"
	}
	return false
}

// ---- handlers

func (s *Server) handleLogin2FA(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(mfaCookie)
	var uid int64
	ok := false
	if err == nil {
		uid, ok = parseMFAToken(c.Value, s.clock())
	}
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if r.Method == http.MethodGet {
		s.render(w, r, "login2fa", pageData{Title: "Two-factor authentication"})
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-site request rejected", http.StatusForbidden)
		return
	}
	u, err := s.store.GetUser(r.Context(), uid)
	if err != nil || u.Disabled {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	keys := loginKeys(r, u.Username)
	if wait := s.logins.Blocked(keys); wait > 0 {
		s.render(w, r, "login2fa", pageData{Title: "Two-factor authentication",
			Error: fmt.Sprintf("Too many failed attempts. Try again in %d minutes.", int(wait.Minutes())+1)})
		return
	}
	usedRecovery, err := s.store.checkSecondFactor(r.Context(), uid, r.FormValue("code"), s.clock())
	if err != nil {
		s.logins.Fail(keys)
		s.auditAs(r, u.Username, "login.2fa_failed", "")
		s.render(w, r, "login2fa", pageData{Title: "Two-factor authentication", Error: "Invalid code. Try the current code from your app."})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: mfaCookie, Value: "", Path: "/login", MaxAge: -1})
	detail := "two-factor"
	if usedRecovery {
		detail = "two-factor with a recovery code"
	}
	s.startSession(w, r, u, detail)
}

// startSession signs the user in and redirects to the dashboard.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u User, how string) {
	s.logins.Success(loginKeys(r, u.Username))
	sess := s.sessionSettings(r.Context())
	tok, err := s.store.newSession(r.Context(), u.ID, sess.Lifetime())
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.auditAs(r, u.Username, "login", u.Source+", role "+u.Role+", "+how)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true, Secure: r.TLS != nil,
		SameSite: http.SameSiteStrictMode, MaxAge: int(sess.Lifetime().Seconds()),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleAccount2FA(w http.ResponseWriter, r *http.Request, user string) {
	me := currentUser(r)
	t, err := s.store.getTwoFactor(r.Context(), me.ID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	data := map[string]any{"Enabled": t.Enabled, "CodesLeft": len(t.Codes),
		"Required": s.sessionSettings(r.Context()).mustUse2FA(me)}
	if !t.Enabled {
		if t.Secret == "" || r.URL.Query().Get("new") == "1" {
			t.Secret = newTOTPSecret()
			if err := s.store.setPendingSecret(r.Context(), me.ID, t.Secret); err != nil {
				s.serverError(w, err)
				return
			}
		}
		q, err := qrFor(totpURI(t.Secret, me.Username))
		if err != nil {
			s.serverError(w, err)
			return
		}
		data["QR"], data["Secret"] = q, groupSecret(t.Secret)
	}
	s.render(w, r, "account2fa", pageData{Title: "Two-factor authentication", Nav: "account", User: user, Data: data})
}

func groupSecret(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && i%4 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (s *Server) handleAccount2FAEnable(w http.ResponseWriter, r *http.Request, user string) {
	me := currentUser(r)
	t, err := s.store.getTwoFactor(r.Context(), me.ID)
	if err != nil || t.Secret == "" {
		redirectErr(w, r, "/account/2fa", errors.New("start the setup again"))
		return
	}
	step, ok := totpVerify(t.Secret, r.FormValue("code"), s.clock(), 0)
	if !ok {
		redirectErr(w, r, "/account/2fa", errors.New("the code does not match — check the time on your phone and try the current code"))
		return
	}
	plain, hashed := newRecoveryCodes()
	if err := s.store.enableTwoFactor(r.Context(), me.ID, step, hashed); err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, "account.2fa_enabled", "")
	s.render(w, r, "account2fa", pageData{Title: "Two-factor authentication", Nav: "account", User: user,
		Flash: "Two-factor authentication is on.", Data: map[string]any{"Enabled": true, "NewCodes": plain, "CodesLeft": len(plain)}})
}

func (s *Server) handleAccount2FACodes(w http.ResponseWriter, r *http.Request, user string) {
	me := currentUser(r)
	if _, err := s.store.checkSecondFactor(r.Context(), me.ID, r.FormValue("code"), s.clock()); err != nil {
		redirectErr(w, r, "/account/2fa", errors.New("enter a current code from your app to create new recovery codes"))
		return
	}
	plain, hashed := newRecoveryCodes()
	if _, err := s.store.db.Exec(r.Context(), `UPDATE users SET totp_recovery=$2 WHERE id=$1`, me.ID, hashed); err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, "account.2fa_codes", "new recovery codes")
	s.render(w, r, "account2fa", pageData{Title: "Two-factor authentication", Nav: "account", User: user,
		Data: map[string]any{"Enabled": true, "NewCodes": plain, "CodesLeft": len(plain)}})
}

func (s *Server) handleAccount2FADisable(w http.ResponseWriter, r *http.Request, _ string) {
	me := currentUser(r)
	if s.sessionSettings(r.Context()).mustUse2FA(me) {
		redirectErr(w, r, "/account/2fa", errors.New("two-factor authentication is required for your account"))
		return
	}
	if _, err := s.store.checkSecondFactor(r.Context(), me.ID, r.FormValue("code"), s.clock()); err != nil {
		redirectErr(w, r, "/account/2fa", errors.New("enter a current code from your app to turn two-factor authentication off"))
		return
	}
	if err := s.store.DisableTwoFactor(r.Context(), me.ID); err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, "account.2fa_disabled", "")
	redirectMsg(w, r, "/account/2fa", "Two-factor authentication is off.")
}

func (s *Server) handleUser2FAReset(w http.ResponseWriter, r *http.Request, _ string) {
	u, ok := s.userFromPath(w, r)
	if !ok {
		return
	}
	if err := s.store.DisableTwoFactor(r.Context(), u.ID); err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, "user.2fa_reset", "%s", u.Username)
	redirectMsg(w, r, fmt.Sprintf("/users/%d", u.ID), "Two-factor authentication reset; the user sets it up again at the next sign-in if required.")
}
