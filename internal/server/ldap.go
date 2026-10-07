package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-ldap/ldap/v3"

	"github.com/max-zit/backupzit/internal/tlsutil"
)

const settingLDAP = "ldap"

// LDAPSettings configure sign-in with Active Directory or another LDAP
// directory. Roles come from group membership at every sign-in.
type LDAPSettings struct {
	Enabled  bool   `json:"enabled"`
	URL      string `json:"url"`      // ldaps://dc1.example.local:636 or ldap://...:389 with StartTLS
	StartTLS bool   `json:"starttls"` // for ldap:// URLs
	// Certificate trust: a CA certificate (PEM) or a pinned server
	// certificate fingerprint; neither = the system's trusted CAs.
	CACert      string `json:"ca_cert,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`

	BindDN       string `json:"bind_dn"`
	BindPassword string `json:"bind_password,omitempty"`
	BaseDN       string `json:"base_dn"`
	UserFilter   string `json:"user_filter"` // {username} is replaced
	NestedGroups bool   `json:"nested_groups"`

	// Group DNs per role, highest role wins.
	AdminGroup    string `json:"admin_group"`
	OperatorGroup string `json:"operator_group"`
	RestoreGroup  string `json:"restore_group"`
	ViewerGroup   string `json:"viewer_group"`
}

const (
	adUserFilter   = "(&(objectClass=user)(sAMAccountName={username}))"
	ldapUserFilter = "(&(objectClass=inetOrgPerson)(uid={username}))"
)

var errNoRole = errors.New("your directory account is not in any BackupZit group")

func (l LDAPSettings) Validate() error {
	if !l.Enabled {
		return nil
	}
	u, err := url.Parse(l.URL)
	if err != nil || (u.Scheme != "ldaps" && u.Scheme != "ldap") || u.Host == "" {
		return errors.New("server URL must look like ldaps://dc1.example.local:636")
	}
	if u.Scheme == "ldap" && !l.StartTLS {
		return errors.New("plain ldap:// sends passwords unencrypted; use ldaps:// or enable StartTLS")
	}
	if l.BaseDN == "" || l.BindDN == "" {
		return errors.New("base DN and bind DN are required")
	}
	if !strings.Contains(l.UserFilter, "{username}") {
		return errors.New("the user filter must contain {username}")
	}
	if l.AdminGroup == "" && l.OperatorGroup == "" && l.RestoreGroup == "" && l.ViewerGroup == "" {
		return errors.New("map at least one directory group to a role")
	}
	if l.CACert != "" {
		if !x509.NewCertPool().AppendCertsFromPEM([]byte(l.CACert)) {
			return errors.New("the CA certificate is not valid PEM")
		}
	}
	if l.Fingerprint != "" && !strings.HasPrefix(l.Fingerprint, "SHA256:") {
		return errors.New("the certificate fingerprint must start with SHA256:")
	}
	return nil
}

func (l LDAPSettings) tlsConfig(host string) *tls.Config {
	if l.Fingerprint != "" {
		c := tlsutil.PinnedConfig(l.Fingerprint)
		c.ServerName = host
		return c
	}
	c := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	if l.CACert != "" {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM([]byte(l.CACert))
		c.RootCAs = pool
	}
	return c
}

func (l LDAPSettings) connect(ctx context.Context) (*ldap.Conn, error) {
	u, err := url.Parse(l.URL)
	if err != nil {
		return nil, err
	}
	host := u.Hostname()
	d := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := ldap.DialURL(l.URL, ldap.DialWithDialer(d), ldap.DialWithTLSConfig(l.tlsConfig(host)))
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", l.URL, err)
	}
	conn.SetTimeout(15 * time.Second)
	if u.Scheme == "ldap" && l.StartTLS {
		if err := conn.StartTLS(l.tlsConfig(host)); err != nil {
			conn.Close()
			return nil, fmt.Errorf("StartTLS: %w", err)
		}
	}
	if err := conn.Bind(l.BindDN, l.BindPassword); err != nil {
		conn.Close()
		return nil, fmt.Errorf("bind as %s: %w", l.BindDN, err)
	}
	return conn, nil
}

// LDAPResult describes an authenticated directory user.
type LDAPResult struct {
	DN, DisplayName, Email, Role string
	Groups                       []string // mapped groups the user is in
}

// Authenticate verifies username/password against the directory and
// determines the role.
func (l LDAPSettings) Authenticate(ctx context.Context, username, password string) (*LDAPResult, error) {
	if password == "" {
		return nil, errBadLogin // an empty password would be an anonymous bind
	}
	conn, err := l.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	res, err := l.lookup(conn, username)
	if err != nil {
		return nil, err
	}
	// Verify the password by binding as the user, then return to the
	// service account for the group checks.
	if err := conn.Bind(res.DN, password); err != nil {
		return nil, errBadLogin
	}
	if err := conn.Bind(l.BindDN, l.BindPassword); err != nil {
		return nil, err
	}
	if err := l.resolveRole(conn, username, res); err != nil {
		return nil, err
	}
	return res, nil
}

func (l LDAPSettings) lookup(conn *ldap.Conn, username string) (*LDAPResult, error) {
	filter := strings.ReplaceAll(l.UserFilter, "{username}", ldap.EscapeFilter(username))
	sr, err := conn.Search(ldap.NewSearchRequest(l.BaseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 2, 15, false,
		filter, []string{"dn", "displayName", "cn", "mail"}, nil))
	if err != nil {
		return nil, fmt.Errorf("search user: %w", err)
	}
	if len(sr.Entries) != 1 {
		return nil, errBadLogin
	}
	e := sr.Entries[0]
	name := e.GetAttributeValue("displayName")
	if name == "" {
		name = e.GetAttributeValue("cn")
	}
	return &LDAPResult{DN: e.DN, DisplayName: name, Email: e.GetAttributeValue("mail")}, nil
}

// inGroup checks membership via the group's member attributes, or with
// AD's transitive matching rule for nested groups.
func (l LDAPSettings) inGroup(conn *ldap.Conn, username, userDN, groupDN string) (bool, error) {
	var base, filter string
	if l.NestedGroups {
		base = userDN
		filter = "(memberOf:1.2.840.113556.1.4.1941:=" + ldap.EscapeFilter(groupDN) + ")"
	} else {
		base = groupDN
		filter = "(|(member=" + ldap.EscapeFilter(userDN) + ")(uniqueMember=" + ldap.EscapeFilter(userDN) + ")(memberUid=" + ldap.EscapeFilter(username) + "))"
	}
	sr, err := conn.Search(ldap.NewSearchRequest(base, ldap.ScopeBaseObject, ldap.NeverDerefAliases, 1, 15, false,
		filter, []string{"dn"}, nil))
	if err != nil {
		if ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchObject) {
			return false, fmt.Errorf("group %s does not exist", groupDN)
		}
		return false, err
	}
	return len(sr.Entries) > 0, nil
}

func (l LDAPSettings) resolveRole(conn *ldap.Conn, username string, res *LDAPResult) error {
	for _, m := range []struct{ role, group string }{
		{"admin", l.AdminGroup}, {"operator", l.OperatorGroup}, {"restore", l.RestoreGroup}, {"viewer", l.ViewerGroup},
	} {
		if strings.TrimSpace(m.group) == "" {
			continue
		}
		ok, err := l.inGroup(conn, username, res.DN, m.group)
		if err != nil {
			return err
		}
		if ok {
			res.Groups = append(res.Groups, m.group)
			if res.Role == "" {
				res.Role = m.role
			}
		}
	}
	if res.Role == "" {
		return errNoRole
	}
	return nil
}

// ---- settings page handlers

func (s *Server) ldapFromForm(r *http.Request) (LDAPSettings, error) {
	var old LDAPSettings
	if err := s.store.GetSetting(r.Context(), settingLDAP, &old); err != nil {
		return old, err
	}
	t := func(k string) string { return strings.TrimSpace(r.FormValue(k)) }
	l := LDAPSettings{
		Enabled: r.FormValue("enabled") == "on", URL: t("url"), StartTLS: r.FormValue("starttls") == "on",
		CACert: t("ca_cert"), Fingerprint: t("fingerprint"),
		BindDN: t("bind_dn"), BindPassword: r.FormValue("bind_password"), BaseDN: t("base_dn"), UserFilter: t("user_filter"),
		NestedGroups: r.FormValue("nested_groups") == "on",
		AdminGroup:   t("admin_group"), OperatorGroup: t("operator_group"), RestoreGroup: t("restore_group"), ViewerGroup: t("viewer_group"),
	}
	if l.BindPassword == "" {
		l.BindPassword = old.BindPassword
	}
	if l.UserFilter == "" {
		l.UserFilter = adUserFilter
	}
	return l, l.Validate()
}

func (s *Server) handleSettingsLDAP(w http.ResponseWriter, r *http.Request, _ string) {
	l, err := s.ldapFromForm(r)
	if err != nil {
		redirectErr(w, r, "/settings/ldap", err)
		return
	}
	if r.FormValue("action") == "test" {
		l.Enabled = true
		if err := l.Validate(); err != nil {
			redirectErr(w, r, "/settings/ldap", err)
			return
		}
		msg, err := l.Test(r.Context(), t2(r.FormValue("test_user")), r.FormValue("test_password"))
		if err != nil {
			redirectErr(w, r, "/settings/ldap", fmt.Errorf("LDAP test failed: %w", err))
			return
		}
		redirectMsg(w, r, "/settings/ldap", msg)
		return
	}
	if s.store.fourEyes(r.Context()) {
		// LDAP groups decide roles: a change could make anyone an approver.
		p, err := s.store.sealPayload(l)
		if err != nil {
			s.serverError(w, err)
			return
		}
		if s.needsApproval(w, r, "/settings/ldap", "settings.ldap", 0, "LDAP "+l.URL, p) {
			return
		}
	}
	if err := s.store.SetSetting(r.Context(), settingLDAP, l); err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, "settings.ldap", "LDAP sign-in enabled=%v url=%s", l.Enabled, l.URL)
	redirectMsg(w, r, "/settings/ldap", "LDAP settings saved.")
}

func t2(s string) string { return strings.TrimSpace(s) }

// Test checks the service account and optionally a user's sign-in.
func (l LDAPSettings) Test(ctx context.Context, username, password string) (string, error) {
	conn, err := l.connect(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if username == "" {
		return "Connected and signed in as the service account. Enter a test user to check sign-in and groups.", nil
	}
	if password == "" {
		res, err := l.lookup(conn, username)
		if err != nil {
			return "", errors.New("user not found with the user filter")
		}
		if err := l.resolveRole(conn, username, res); err != nil {
			return "", fmt.Errorf("found %s, but: %w", res.DN, err)
		}
		return fmt.Sprintf("Found %s; role %s (password not checked).", res.DN, roleTitle(res.Role)), nil
	}
	conn.Close()
	res, err := l.Authenticate(ctx, username, password)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Signed in as %s (%s); role %s.", res.DN, res.DisplayName, roleTitle(res.Role)), nil
}

func roleTitle(k string) string {
	if r, ok := roleByKey(k); ok {
		return r.Name
	}
	return k
}

// ldapPage is LDAPSettings without the bind password, for the form.
type ldapPage struct {
	LDAPSettings
	HasPassword bool
}

func ldapForPage(ctx context.Context, st *Store) ldapPage {
	var l LDAPSettings
	st.GetSetting(ctx, settingLDAP, &l)
	if l.UserFilter == "" {
		l.UserFilter = adUserFilter
	}
	if l.URL == "" {
		l.URL = "ldaps://"
	}
	p := ldapPage{LDAPSettings: l, HasPassword: l.BindPassword != ""}
	p.BindPassword = ""
	return p
}
