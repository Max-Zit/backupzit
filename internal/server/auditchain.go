package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Tamper-evident audit log. Every entry carries a hash over its content and
// the previous entry's hash, keyed with the console's secrets key (a file
// on the server, not in the database): changing or deleting an entry in
// the database breaks the chain, and nobody without the key can compute a
// valid chain again. The newest entry's hash is also written to a file
// next to the key, so deleting the last entries shows as well. Entries can
// be sent to a syslog server as they are written, for a copy off the
// server.

const auditLock = 7342881 // pg advisory lock serialising appends

// auditHash is the HMAC of an entry and the previous hash.
func auditHash(key []byte, id int64, at time.Time, user, action, detail, remote, prev string) string {
	if key == nil {
		key = []byte("backupzit audit chain (no secrets key)")
	}
	m := hmac.New(sha256.New, key)
	fmt.Fprintf(m, "%d\n%s\n%s\n%s\n%s\n%s\n%s", id, at.UTC().Format(time.RFC3339Nano), user, action, detail, remote, prev)
	return hex.EncodeToString(m.Sum(nil))
}

// auditHead is the newest entry of the chain, kept outside the database.
type auditHead struct {
	ID   int64  `json:"id"`
	Hash string `json:"hash"`
}

// AppendAudit adds an entry to the audit log and extends the chain.
func (s *Store) AppendAudit(ctx context.Context, user, action, detail, remote string) (AuditEntry, error) {
	e := AuditEntry{Username: user, Action: action, Detail: clip(detail, 2000), Remote: remote,
		At: time.Now().UTC().Truncate(time.Microsecond)} // PostgreSQL keeps microseconds
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return e, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, auditLock); err != nil {
		return e, err
	}
	var prev string
	var lastID int64
	if err := tx.QueryRow(ctx, `SELECT id, hash FROM audit_log WHERE hash <> '' ORDER BY id DESC LIMIT 1`).Scan(&lastID, &prev); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return e, err
	}
	// When the newest entries were deleted, chain to the head kept outside
	// the database, so the gap stays visible after the next entry.
	if h, ok := s.readAuditHead(); ok && h.ID > lastID {
		prev = h.Hash
	}
	if err := tx.QueryRow(ctx, `SELECT nextval(pg_get_serial_sequence('audit_log', 'id'))`).Scan(&e.ID); err != nil {
		return e, err
	}
	e.PrevHash = prev
	e.Hash = auditHash(s.auditKey, e.ID, e.At, e.Username, e.Action, e.Detail, e.Remote, prev)
	if _, err := tx.Exec(ctx, `INSERT INTO audit_log(id, at, username, action, detail, remote, prev_hash, hash) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
		e.ID, e.At, e.Username, e.Action, e.Detail, e.Remote, e.PrevHash, e.Hash); err != nil {
		return e, err
	}
	if err := tx.Commit(ctx); err != nil {
		return e, err
	}
	if s.auditHeadFile != "" {
		b, _ := json.Marshal(auditHead{ID: e.ID, Hash: e.Hash})
		tmp := s.auditHeadFile + ".tmp"
		if os.WriteFile(tmp, b, 0o600) == nil {
			os.Rename(tmp, s.auditHeadFile)
		}
	}
	if s.auditSink != nil {
		s.auditSink(e)
	}
	return e, nil
}

// AuditIntegrity is the result of checking the chain.
type AuditIntegrity struct {
	Checked time.Time
	OK      bool
	Entries int   // entries in the chain
	Legacy  int   // older entries without a hash (before the chain existed)
	BadID   int64 // first entry where the chain breaks
	Problem string
}

// VerifyAudit checks the whole chain and the head kept outside the
// database.
func (s *Store) VerifyAudit(ctx context.Context) AuditIntegrity {
	res := AuditIntegrity{Checked: time.Now(), OK: true}
	fail := func(id int64, format string, args ...any) AuditIntegrity {
		res.OK, res.BadID, res.Problem = false, id, fmt.Sprintf(format, args...)
		return res
	}
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE hash = ''`).Scan(&res.Legacy); err != nil {
		return fail(0, "cannot read the audit log: %v", err)
	}
	rows, err := s.db.Query(ctx, `SELECT id, at, username, action, detail, remote, prev_hash, hash FROM audit_log WHERE hash <> '' ORDER BY id`)
	if err != nil {
		return fail(0, "cannot read the audit log: %v", err)
	}
	defer rows.Close()
	prev, last := "", int64(0)
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.At, &e.Username, &e.Action, &e.Detail, &e.Remote, &e.PrevHash, &e.Hash); err != nil {
			return fail(0, "cannot read the audit log: %v", err)
		}
		if e.PrevHash != prev {
			return fail(e.ID, "entries before #%d were deleted or changed", e.ID)
		}
		if auditHash(s.auditKey, e.ID, e.At, e.Username, e.Action, e.Detail, e.Remote, e.PrevHash) != e.Hash {
			return fail(e.ID, "entry #%d was changed", e.ID)
		}
		prev, last = e.Hash, e.ID
		res.Entries++
	}
	if err := rows.Err(); err != nil {
		return fail(0, "cannot read the audit log: %v", err)
	}
	if h, ok := s.readAuditHead(); ok && (h.ID > last || (h.ID == last && h.Hash != prev)) {
		return fail(h.ID, "the newest entries (up to #%d) were deleted", h.ID)
	}
	return res
}

func (s *Store) readAuditHead() (auditHead, bool) {
	var h auditHead
	if s.auditHeadFile == "" {
		return h, false
	}
	b, err := os.ReadFile(s.auditHeadFile)
	if err != nil || json.Unmarshal(b, &h) != nil || h.ID <= 0 {
		return h, false
	}
	return h, true
}

// ---- syslog export

const settingAuditSyslog = "audit_syslog"

// AuditSyslog sends every audit entry to a syslog server (RFC 5424).
type AuditSyslog struct {
	Address  string `json:"address,omitempty"`  // host:port
	Protocol string `json:"protocol,omitempty"` // udp, tcp or tls
}

func (a *AuditSyslog) Validate() error {
	if a.Address == "" {
		return nil
	}
	host, port, err := net.SplitHostPort(a.Address)
	if err != nil || host == "" || port == "" {
		return errors.New("enter the syslog server as host:port, e.g. siem.example.com:514")
	}
	switch a.Protocol {
	case "udp", "tcp", "tls":
	default:
		return errors.New("choose UDP, TCP or TLS for the syslog server")
	}
	return nil
}

// syslogLine formats an entry as an RFC 5424 message (facility auth,
// severity notice) with the chain hash, so the copy can be compared.
func syslogLine(host string, e AuditEntry) string {
	esc := func(s string) string { return strings.NewReplacer(`\`, `\\`, `"`, `\"`, `]`, `\]`).Replace(s) }
	return fmt.Sprintf(`<37>1 %s %s backupzit - audit [audit@32473 id="%d" user="%s" action="%s" remote="%s" hash="%s"] %s`,
		e.At.UTC().Format(time.RFC3339Nano), host, e.ID, esc(e.Username), esc(e.Action), esc(e.Remote), e.Hash,
		strings.ReplaceAll(e.Detail, "\n", " "))
}

type syslogSender struct {
	mu   sync.Mutex
	cfg  AuditSyslog
	conn net.Conn
	q    chan AuditEntry
	log  func(msg string, kv ...any)
}

func (x *syslogSender) send(line string) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.cfg.Address == "" {
		return nil
	}
	if x.conn != nil && x.cfg.Protocol != "udp" && !streamOpen(x.conn) {
		x.conn.Close() // the server closed it: a write would be lost
		x.conn = nil
	}
	if x.conn == nil {
		var c net.Conn
		var err error
		switch x.cfg.Protocol {
		case "tls":
			// Assigned through c: a failed tls dial returns a nil *tls.Conn,
			// which would be a non-nil net.Conn.
			var tc *tls.Conn
			tc, err = tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", x.cfg.Address, &tls.Config{MinVersion: tls.VersionTLS12})
			if err == nil {
				c = tc
			}
		default:
			c, err = net.DialTimeout(x.cfg.Protocol, x.cfg.Address, 10*time.Second)
		}
		if err != nil {
			return err
		}
		x.conn = c
	}
	msg := line
	if x.cfg.Protocol != "udp" {
		msg = fmt.Sprintf("%d %s", len(line), line) // octet counting (RFC 6587)
	}
	x.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := x.conn.Write([]byte(msg)); err != nil {
		x.conn.Close()
		x.conn = nil
		return err
	}
	return nil
}

// streamOpen reports whether the syslog server still keeps the connection
// open. Syslog servers never send, so a read that times out means open; a
// write to a connection the server closed would succeed and be lost.
func streamOpen(c net.Conn) bool {
	c.SetReadDeadline(time.Now().Add(time.Millisecond))
	_, err := c.Read(make([]byte, 1))
	c.SetReadDeadline(time.Time{})
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func (x *syslogSender) configure(c AuditSyslog) {
	x.mu.Lock()
	if x.conn != nil && x.cfg != c {
		x.conn.Close()
		x.conn = nil
	}
	x.cfg = c
	x.mu.Unlock()
}

func (x *syslogSender) run(ctx context.Context, host string) {
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-x.q:
			// Keep the entry until the server takes it; newer entries wait
			// in the queue meanwhile (the database keeps them in any case).
			for wait := time.Second; ; wait = min(2*wait, time.Minute) {
				err := x.send(syslogLine(host, e))
				if err == nil {
					break
				}
				if wait == time.Second {
					x.log("audit log: syslog server not reachable", "err", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(wait):
				}
			}
		}
	}
}

// StartAuditChain keeps the chain's head next to the secrets key, checks
// the chain at the start and daily, and sends entries to the syslog
// server (Settings → Sign-in & security).
func (s *Server) StartAuditChain(ctx context.Context, dataDir string) {
	s.store.auditHeadFile = dataDir + string(os.PathSeparator) + "audit-head.json"
	host := hostname()
	s.syslog = &syslogSender{q: make(chan AuditEntry, 1000), log: s.log.Warn}
	var c AuditSyslog
	s.store.GetSetting(ctx, settingAuditSyslog, &c)
	s.syslog.configure(c)
	s.store.auditSink = func(e AuditEntry) {
		select {
		case s.syslog.q <- e:
		default: // the server is slow or down: the database keeps the entry
		}
	}
	go s.syslog.run(ctx, host)
	go func() {
		for {
			res := s.store.VerifyAudit(ctx)
			s.auditState.Store(&res)
			if !res.OK {
				s.log.Error("audit log integrity check failed", "problem", res.Problem)
				s.mailAdmins(ctx, "[BackupZit] The audit log was changed",
					"The integrity check of the audit log failed: "+res.Problem+".\n\nSomebody with access to the database changed or deleted audit entries. Check the server and its database access.\n")
			}
			// Daily checkpoint to the syslog copy.
			if res.OK && res.Entries > 0 {
				s.store.auditSink(AuditEntry{At: time.Now().UTC(), Action: "audit.checkpoint", Detail: fmt.Sprintf("chain verified, %d entries", res.Entries), Hash: s.store.auditHeadHash(ctx)})
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(24 * time.Hour):
			}
		}
	}()
}

func (s *Store) auditHeadHash(ctx context.Context) string {
	var h string
	s.db.QueryRow(ctx, `SELECT hash FROM audit_log WHERE hash <> '' ORDER BY id DESC LIMIT 1`).Scan(&h)
	return h
}

// handleAuditVerify checks the chain now (Audit log page).
func (s *Server) handleAuditVerify(w http.ResponseWriter, r *http.Request, user string) {
	res := s.store.VerifyAudit(r.Context())
	s.auditState.Store(&res)
	if !res.OK {
		redirectErr(w, r, "/audit", errors.New("the audit log was changed: "+res.Problem))
		return
	}
	redirectMsg(w, r, "/audit", "The audit log is intact.")
}

func (s *Server) handleSettingsAuditSyslog(w http.ResponseWriter, r *http.Request, user string) {
	const back = "/settings/security#audit-syslog"
	c := AuditSyslog{Address: strings.TrimSpace(r.FormValue("address")), Protocol: r.FormValue("protocol")}
	if c.Address == "" {
		c.Protocol = ""
	}
	if err := c.Validate(); err != nil {
		redirectErr(w, r, back, err)
		return
	}
	if err := s.store.SetSetting(r.Context(), settingAuditSyslog, c); err != nil {
		s.serverError(w, err)
		return
	}
	if s.syslog != nil {
		s.syslog.configure(c)
	}
	s.audit(r, "settings.audit_syslog", "syslog %s %s", c.Protocol, c.Address)
	if c.Address != "" && s.syslog != nil {
		if err := s.syslog.send(syslogLine(hostname(), AuditEntry{At: time.Now().UTC(), Action: "audit.test", Detail: "test message from the BackupZit console"})); err != nil {
			redirectErr(w, r, back, fmt.Errorf("saved, but the syslog server did not take the test message: %w", err))
			return
		}
	}
	redirectMsg(w, r, back, "Audit log export saved.")
}

func (s *Server) auditSyslogSettings(ctx context.Context) AuditSyslog {
	var c AuditSyslog
	if s.store != nil {
		s.store.GetSetting(ctx, settingAuditSyslog, &c)
	}
	return c
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}
