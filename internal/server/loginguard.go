package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Brute-force protection of the sign-in. Failed sign-ins (password, second
// factor, API tokens) are counted per client address and per username in
// memory. An address that reaches the limit is blocked in the database, so
// a restart does not release it; repeated offenders are blocked for twice
// as long each time. A username that fails too often from many addresses
// is only throttled for the window, so a guesser cannot lock out the
// administrator for long.

const settingLoginProtection = "login_protection"

// LoginProtection are the brute-force settings (Settings → Sign-in & security).
type LoginProtection struct {
	// MaxAttempts failed sign-ins from one address within WindowMinutes
	// block that address for BlockMinutes.
	MaxAttempts   int `json:"max_attempts"`
	WindowMinutes int `json:"window_minutes"`
	BlockMinutes  int `json:"block_minutes"`
	// Progressive doubles the block for every repeated block within a day,
	// up to maxBlock.
	Progressive bool `json:"progressive"`
	// MaxUserAttempts failed sign-ins for one username (from any address)
	// within the window refuse further attempts for it until the window passes.
	MaxUserAttempts int `json:"max_user_attempts"`
	// TrustedNetworks are never blocked (CIDRs or single addresses).
	TrustedNetworks []string `json:"trusted_networks,omitempty"`
	// TrustedProxies are reverse proxies whose X-Forwarded-For header
	// names the real client address.
	TrustedProxies []string `json:"trusted_proxies,omitempty"`
	// Notify sends an email when an address is blocked.
	Notify bool `json:"notify"`
}

var defaultLoginProtection = LoginProtection{MaxAttempts: 5, WindowMinutes: 15, BlockMinutes: 30, Progressive: true, MaxUserAttempts: 20, Notify: true}

const maxBlock = 7 * 24 * time.Hour

func (x LoginProtection) Validate() error {
	switch {
	case x.MaxAttempts < 3 || x.MaxAttempts > 100:
		return errors.New("failed attempts per address must be between 3 and 100")
	case x.WindowMinutes < 1 || x.WindowMinutes > 1440:
		return errors.New("the counting period must be between 1 minute and 24 hours")
	case x.BlockMinutes < 1 || x.BlockMinutes > 10080:
		return errors.New("the block must last between 1 minute and 7 days")
	case x.MaxUserAttempts < x.MaxAttempts || x.MaxUserAttempts > 1000:
		return errors.New("failed attempts per username must be between the per-address limit and 1000")
	}
	for _, list := range [][]string{x.TrustedNetworks, x.TrustedProxies} {
		if len(list) > 50 {
			return errors.New("at most 50 trusted networks or proxies")
		}
		for _, n := range list {
			if _, err := parsePrefix(n); err != nil {
				return fmt.Errorf("%q is not an IP address or network (e.g. 192.168.1.0/24)", n)
			}
		}
	}
	return nil
}

func (x LoginProtection) Window() time.Duration { return time.Duration(x.WindowMinutes) * time.Minute }

// blockFor is the length of the strikes-th block of an address.
func (x LoginProtection) blockFor(strikes int) time.Duration {
	d := time.Duration(x.BlockMinutes) * time.Minute
	if x.Progressive {
		for i := 1; i < strikes && d < maxBlock; i++ {
			d *= 2
		}
	}
	return min(d, maxBlock)
}

// parsePrefix accepts "10.0.0.0/8", "192.168.1.5" and IPv6 forms.
func parsePrefix(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		return p.Masked(), err
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()), nil
}

func inPrefixes(ip string, list []string) bool {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	a = a.Unmap()
	for _, n := range list {
		if p, err := parsePrefix(n); err == nil && p.Contains(a) {
			return true
		}
	}
	return false
}

// splitNetworks reads a textarea of networks (one per line or comma separated).
func splitNetworks(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == ',' || r == ' ' || r == '\r' || r == '\t' }) {
		out = append(out, f)
	}
	return out
}

// loginGuard counts failures and blocks addresses.
type loginGuard struct {
	store *Store
	now   func() time.Time

	mu    sync.Mutex
	fails map[string][]time.Time
	// cached settings
	cfg   LoginProtection
	cfgAt time.Time

	// onBlock is called (asynchronously) when an address is blocked.
	onBlock func(ip string, until time.Time, strikes int, reason string)
}

func newLoginGuard(store *Store, now func() time.Time) *loginGuard {
	return &loginGuard{store: store, now: now, fails: map[string][]time.Time{}}
}

func (g *loginGuard) settings(ctx context.Context) LoginProtection {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.settingsLocked(ctx)
}

func (g *loginGuard) settingsLocked(ctx context.Context) LoginProtection {
	if time.Since(g.cfgAt) < 30*time.Second {
		return g.cfg
	}
	if g.store == nil {
		return defaultLoginProtection
	}
	x := defaultLoginProtection
	if err := g.store.GetSetting(ctx, settingLoginProtection, &x); err != nil || x.Validate() != nil {
		x = defaultLoginProtection
	}
	g.cfg, g.cfgAt = x, time.Now()
	return x
}

func (g *loginGuard) reload() {
	g.mu.Lock()
	g.cfgAt = time.Time{}
	g.mu.Unlock()
}

// ClientIP is the address of the browser: the connection's address, or for
// a trusted reverse proxy the last address in X-Forwarded-For that is not
// itself a trusted proxy.
func (g *loginGuard) ClientIP(r *http.Request) string {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	if a, err := netip.ParseAddr(ip); err == nil {
		ip = a.Unmap().String()
	}
	cfg := g.settings(r.Context())
	if len(cfg.TrustedProxies) == 0 || !inPrefixes(ip, cfg.TrustedProxies) {
		return ip
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		h := strings.TrimSpace(hops[i])
		a, err := netip.ParseAddr(h)
		if err != nil {
			break
		}
		ip = a.Unmap().String()
		if !inPrefixes(ip, cfg.TrustedProxies) {
			break
		}
	}
	return ip
}

func userKey(username string) string { return "user:" + strings.ToLower(strings.TrimSpace(username)) }

// recent drops failures older than the window and returns the rest.
func (g *loginGuard) recent(key string, window time.Duration) []time.Time {
	cut := g.now().Add(-window)
	f := g.fails[key]
	i := 0
	for i < len(f) && f[i].Before(cut) {
		i++
	}
	f = f[i:]
	if len(f) == 0 {
		delete(g.fails, key)
		return nil
	}
	g.fails[key] = f
	return f
}

// Blocked returns how long a sign-in from r for username has to wait, or 0.
// An empty username checks only the address.
func (g *loginGuard) Blocked(r *http.Request, username string) time.Duration {
	ctx := r.Context()
	ip := g.ClientIP(r)
	cfg := g.settings(ctx)
	var wait time.Duration
	if !inPrefixes(ip, cfg.TrustedNetworks) && g.store != nil {
		var until time.Time
		err := g.store.db.QueryRow(ctx, `SELECT until FROM login_blocks WHERE ip=$1`, ip).Scan(&until)
		if err == nil && until.After(g.now()) {
			wait = until.Sub(g.now())
		}
	}
	if username != "" {
		g.mu.Lock()
		if f := g.recent(userKey(username), cfg.Window()); len(f) >= cfg.MaxUserAttempts {
			if w := f[len(f)-cfg.MaxUserAttempts].Add(cfg.Window()).Sub(g.now()); w > wait {
				wait = w
			}
		}
		g.mu.Unlock()
	}
	return wait
}

// Fail records a failed sign-in and blocks the address when it reached the limit.
func (g *loginGuard) Fail(r *http.Request, username, what string) {
	ctx := r.Context()
	ip := g.ClientIP(r)
	cfg := g.settings(ctx)
	g.mu.Lock()
	if len(g.fails) > 10000 { // bound memory under a distributed attack
		for k := range g.fails {
			g.recent(k, cfg.Window())
		}
	}
	now := g.now()
	if username != "" {
		k := userKey(username)
		g.fails[k] = append(g.recent(k, cfg.Window()), now)
	}
	block := false
	if !inPrefixes(ip, cfg.TrustedNetworks) {
		k := "ip:" + ip
		g.fails[k] = append(g.recent(k, cfg.Window()), now)
		if len(g.fails[k]) >= cfg.MaxAttempts {
			block = true
			delete(g.fails, k)
		}
	}
	g.mu.Unlock()
	if !block || g.store == nil {
		return
	}
	reason := fmt.Sprintf("%d failed %s", cfg.MaxAttempts, what)
	if username != "" {
		reason += fmt.Sprintf(", last as %q", clip(username, 64))
	}
	until, strikes, err := g.store.blockAddress(ctx, ip, cfg, now, reason)
	if err != nil {
		return
	}
	if g.onBlock != nil {
		go g.onBlock(ip, until, strikes, reason)
	}
}

// Success clears the failures of the address and the username.
func (g *loginGuard) Success(r *http.Request, username string) {
	ip := g.ClientIP(r)
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.fails, "ip:"+ip)
	delete(g.fails, userKey(username))
}

// LoginBlock is a blocked address.
type LoginBlock struct {
	IP        string
	Until     time.Time
	Strikes   int
	Reason    string
	BlockedAt time.Time
}

// blockAddress blocks ip; an address blocked again within a day of its last
// block counts one more strike.
func (s *Store) blockAddress(ctx context.Context, ip string, cfg LoginProtection, now time.Time, reason string) (time.Time, int, error) {
	strikes := 1
	var prevUntil time.Time
	var prev int
	err := s.db.QueryRow(ctx, `SELECT until, strikes FROM login_blocks WHERE ip=$1`, ip).Scan(&prevUntil, &prev)
	if err == nil && now.Sub(prevUntil) < 24*time.Hour {
		strikes = prev + 1
	} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, 0, err
	}
	until := now.Add(cfg.blockFor(strikes))
	_, err = s.db.Exec(ctx, `INSERT INTO login_blocks(ip, until, strikes, reason, blocked_at) VALUES($1,$2,$3,$4,$5)
		ON CONFLICT (ip) DO UPDATE SET until=$2, strikes=$3, reason=$4, blocked_at=$5`, ip, until, strikes, clip(reason, 300), now)
	return until, strikes, err
}

// ListLoginBlocks returns the addresses blocked now and forgets blocks that
// ended more than a day ago (their strikes no longer count).
func (s *Store) ListLoginBlocks(ctx context.Context, now time.Time) ([]LoginBlock, error) {
	if _, err := s.db.Exec(ctx, `DELETE FROM login_blocks WHERE until < $1`, now.Add(-24*time.Hour)); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT ip, until, strikes, reason, blocked_at FROM login_blocks WHERE until > $1 ORDER BY blocked_at DESC`, now)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (LoginBlock, error) {
		var b LoginBlock
		err := r.Scan(&b.IP, &b.Until, &b.Strikes, &b.Reason, &b.BlockedAt)
		return b, err
	})
}

// UnblockAddress lifts the block of ip ("all" lifts every block) and forgets its strikes.
func (s *Store) UnblockAddress(ctx context.Context, ip string) (int64, error) {
	var tag interface{ RowsAffected() int64 }
	var err error
	if ip == "all" {
		tag, err = s.db.Exec(ctx, `DELETE FROM login_blocks`)
	} else {
		tag, err = s.db.Exec(ctx, `DELETE FROM login_blocks WHERE ip=$1`, ip)
	}
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// notifyBlock writes the audit log and, if enabled, emails the administrators.
func (s *Server) notifyBlock(ip string, until time.Time, strikes int, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	detail := fmt.Sprintf("%s blocked until %s (block %d): %s", ip, until.Local().Format("2006-01-02 15:04"), strikes, reason)
	if _, err := s.store.AppendAudit(ctx, "", "security.ip_blocked", detail, ip); err != nil {
		s.log.Error("audit log", "err", err)
	}
	s.log.Warn("address blocked after failed sign-ins", "ip", ip, "until", until, "strikes", strikes, "reason", reason)
	if !s.guard.settings(ctx).Notify {
		return
	}
	var e EmailSettings
	if err := s.store.GetSetting(ctx, settingEmail, &e); err != nil || !e.Enabled || e.Host == "" || len(e.To) == 0 {
		return
	}
	body := fmt.Sprintf("BackupZit blocked the address %s after repeated failed sign-ins to the console.\n\n"+
		"Reason:        %s\nBlocked until: %s\nBlock number:  %d (repeated blocks last longer)\n\n"+
		"If this was you or a colleague, lift the block under Settings → Sign-in & security.\n"+
		"Otherwise someone is trying to guess passwords: make sure the console is not reachable from the internet "+
		"and that every account has a strong password and two-factor authentication.\n",
		ip, reason, until.Local().Format("2006-01-02 15:04:05"), strikes)
	if err := sendMail(ctx, e, "[BackupZit] Sign-in blocked for "+ip, body); err != nil {
		s.log.Error("email about blocked address", "err", err)
	}
}

func (s *Server) handleSettingsLoginProtection(w http.ResponseWriter, r *http.Request, user string) {
	num := func(name string) (int, error) { return strconv.Atoi(strings.TrimSpace(r.FormValue(name))) }
	var x LoginProtection
	var errs [4]error
	x.MaxAttempts, errs[0] = num("max_attempts")
	x.WindowMinutes, errs[1] = num("window_minutes")
	x.BlockMinutes, errs[2] = num("block_minutes")
	x.MaxUserAttempts, errs[3] = num("max_user_attempts")
	for _, err := range errs {
		if err != nil {
			redirectErr(w, r, "/settings/security", errors.New("enter whole numbers"))
			return
		}
	}
	x.Progressive = r.FormValue("progressive") == "on"
	x.Notify = r.FormValue("notify") == "on"
	x.TrustedNetworks = splitNetworks(r.FormValue("trusted_networks"))
	x.TrustedProxies = splitNetworks(r.FormValue("trusted_proxies"))
	if err := x.Validate(); err != nil {
		redirectErr(w, r, "/settings/security", err)
		return
	}
	if err := s.store.SetSetting(r.Context(), settingLoginProtection, x); err != nil {
		s.serverError(w, err)
		return
	}
	s.guard.reload()
	s.audit(r, "settings.login_protection", "%d attempts / %d min, block %d min (progressive %v), %d per username, trusted %v, proxies %v",
		x.MaxAttempts, x.WindowMinutes, x.BlockMinutes, x.Progressive, x.MaxUserAttempts, x.TrustedNetworks, x.TrustedProxies)
	redirectMsg(w, r, "/settings/security", "Brute-force protection saved.")
}

func (s *Server) handleUnblockAddress(w http.ResponseWriter, r *http.Request, _ string) {
	ip := strings.TrimSpace(r.FormValue("ip"))
	if ip != "all" {
		if _, err := netip.ParseAddr(ip); err != nil {
			redirectErr(w, r, "/settings/security", errors.New("invalid address"))
			return
		}
	}
	n, err := s.store.UnblockAddress(r.Context(), ip)
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, "security.ip_unblocked", "%s", ip)
	redirectMsg(w, r, "/settings/security", fmt.Sprintf("%d address(es) unblocked.", n))
}

// Secure tells whether the browser reached the console over HTTPS: directly,
// or through a trusted reverse proxy that says so in X-Forwarded-Proto.
// Cookies of such requests are marked Secure.
func (g *loginGuard) Secure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	if a, err := netip.ParseAddr(ip); err == nil {
		ip = a.Unmap().String()
	}
	cfg := g.settings(r.Context())
	return len(cfg.TrustedProxies) > 0 && inPrefixes(ip, cfg.TrustedProxies) &&
		strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}
