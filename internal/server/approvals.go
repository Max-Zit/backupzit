package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/max-zit/backupzit/internal/repo"
)

// Four-eyes approval (optional, off by default): with it on, deleting
// jobs, agents, ESXi hosts, storage targets and users, shortening a job's
// retention, resuming retention after a suspicious backup and turning the
// approval off again only take effect when a second user approves them.
// A single stolen or misused account then cannot remove the backups'
// configuration or let retention delete them.

const settingFourEyes = "four_eyes"

// approvalTTL is how long a request waits for a decision.
const approvalTTL = 7 * 24 * time.Hour

// approvalAction describes a change that needs approval.
type approvalAction struct {
	Title string // English, translated in the console
	Perm  Perm   // the approver needs it too
}

var approvalActions = map[string]approvalAction{
	"job.delete":       {"Delete backup job", PermJobs},
	"job.retention":    {"Shorten retention", PermJobs},
	"job.release_hold": {"Resume retention after a suspicious backup", PermJobs},
	"agent.delete":     {"Remove agent", PermAgents},
	"vmware.delete":    {"Remove ESXi host", PermAgents},
	"storage.delete":   {"Remove storage target", PermStorage},
	"user.delete":      {"Delete user", PermUsers},
	"four_eyes.off":    {"Turn off four-eyes approval", PermSettings},
	"user.create":      {"Create a user who can approve", PermUsers},
	"user.role":        {"Give a user more rights", PermUsers},
	"user.password":    {"Reset the password of a user who can approve", PermUsers},
	"user.2fa_reset":   {"Reset the two-factor sign-in of a user who can approve", PermUsers},
	"settings.ldap":    {"Change LDAP sign-in", PermSettings},
	"settings.sso":     {"Change single sign-on", PermSettings},
	"job.disable":      {"Disable backup job", PermJobs},
}

// Approval is a change waiting for (or decided by) a second user.
type Approval struct {
	ID          int64
	Action      string
	ObjectID    int64
	ObjectName  string
	Payload     json.RawMessage
	RequestedBy string
	RequestedAt time.Time
	Status      string // pending, approved, rejected, cancelled, expired, failed
	DecidedBy   *string
	DecidedAt   *time.Time
	Note        string
}

// Title is the action's English title.
func (a Approval) Title() string { return approvalActions[a.Action].Title }

// Expires is when a pending request lapses.
func (a Approval) Expires() time.Time { return a.RequestedAt.Add(approvalTTL) }

// Policy is the requested retention of a job.retention request.
func (a Approval) Policy() repo.RetentionPolicy {
	var p repo.RetentionPolicy
	json.Unmarshal(a.Payload, &p)
	return p
}

func (s *Store) fourEyes(ctx context.Context) bool {
	var on bool
	s.GetSetting(ctx, settingFourEyes, &on)
	return on
}

const approvalCols = `id, action, object_id, object_name, payload, requested_by, requested_at, status, decided_by, decided_at, note`

func scanApproval(r pgx.CollectableRow) (Approval, error) {
	var a Approval
	err := r.Scan(&a.ID, &a.Action, &a.ObjectID, &a.ObjectName, &a.Payload, &a.RequestedBy, &a.RequestedAt, &a.Status, &a.DecidedBy, &a.DecidedAt, &a.Note)
	return a, err
}

// expireApprovals marks requests older than approvalTTL as expired.
func (s *Store) expireApprovals(ctx context.Context) {
	s.db.Exec(ctx, `UPDATE approvals SET status='expired', decided_at=now() WHERE status='pending' AND requested_at < $1`, time.Now().Add(-approvalTTL))
}

// ListApprovals returns the pending requests and the newest 50 decided ones.
func (s *Store) ListApprovals(ctx context.Context) (pending, decided []Approval, err error) {
	s.expireApprovals(ctx)
	rows, err := s.db.Query(ctx, `SELECT `+approvalCols+` FROM approvals WHERE status='pending' ORDER BY requested_at`)
	if err != nil {
		return nil, nil, err
	}
	if pending, err = pgx.CollectRows(rows, scanApproval); err != nil {
		return nil, nil, err
	}
	rows, err = s.db.Query(ctx, `SELECT `+approvalCols+` FROM approvals WHERE status<>'pending' ORDER BY decided_at DESC NULLS LAST, id DESC LIMIT 50`)
	if err != nil {
		return nil, nil, err
	}
	decided, err = pgx.CollectRows(rows, scanApproval)
	return pending, decided, err
}

// PendingApprovals counts the requests waiting for a decision.
func (s *Store) PendingApprovals(ctx context.Context) int {
	var n int
	s.db.QueryRow(ctx, `SELECT count(*) FROM approvals WHERE status='pending' AND requested_at >= $1`, time.Now().Add(-approvalTTL)).Scan(&n)
	return n
}

func (s *Store) getApproval(ctx context.Context, id int64) (Approval, error) {
	rows, err := s.db.Query(ctx, `SELECT `+approvalCols+` FROM approvals WHERE id=$1`, id)
	if err != nil {
		return Approval{}, err
	}
	return pgx.CollectExactlyOneRow(rows, scanApproval)
}

// errAlreadyRequested reports a request for the same change that is still
// pending.
var errAlreadyRequested = errors.New("this change is already waiting for approval")

// requestApproval records a request. It fails when the same change is
// already pending.
func (s *Store) requestApproval(ctx context.Context, a Approval) error {
	s.expireApprovals(ctx)
	if a.Payload == nil {
		a.Payload = json.RawMessage(`{}`)
	}
	ct, err := s.db.Exec(ctx, `INSERT INTO approvals(action, object_id, object_name, payload, requested_by)
		VALUES($1,$2,$3,$4,$5) ON CONFLICT (action, object_id) WHERE status='pending' DO NOTHING`,
		a.Action, a.ObjectID, a.ObjectName, a.Payload, a.RequestedBy)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return errAlreadyRequested
	}
	return nil
}

// decideApproval moves a pending request to a final status; it fails when
// another user decided it first.
func (s *Store) decideApproval(ctx context.Context, id int64, status, by, note string) error {
	ct, err := s.db.Exec(ctx, `UPDATE approvals SET status=$2, decided_by=$3, decided_at=now(), note=$4 WHERE id=$1 AND status='pending'`, id, status, by, note)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return errors.New("this request was already decided")
	}
	return nil
}

// needsApproval is called by the handlers of the protected changes. With
// four-eyes approval on it records the request, tells the administrators and
// answers the browser, and returns true: the handler then stops.
func (s *Server) needsApproval(w http.ResponseWriter, r *http.Request, back, action string, objectID int64, objectName string, payload any) bool {
	ctx := r.Context()
	if !s.store.fourEyes(ctx) {
		return false
	}
	me := currentUser(r)
	a := Approval{Action: action, ObjectID: objectID, ObjectName: objectName, RequestedBy: me.Username}
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			s.serverError(w, err)
			return true
		}
		a.Payload = b
	}
	if err := s.store.requestApproval(ctx, a); err != nil {
		redirectErr(w, r, back, err)
		return true
	}
	s.audit(r, "approval.request", "%s: %s", a.Title(), objectName)
	s.mailAdmins(ctx, "BackupZit: approval needed — "+a.Title(),
		fmt.Sprintf("%s requested: %s (%s).\n\nAnother user must approve or reject it in the console under Approvals:\n%s/approvals\n\nThe request expires after 7 days.",
			me.Username, a.Title(), objectName, s.PublicURL))
	redirectMsg(w, r, back, "Waiting for approval: another user must approve this change under Approvals.")
	return true
}

// executeApproval carries out an approved change.
func (s *Server) executeApproval(ctx context.Context, a Approval) error {
	if ok, err := s.executeUserApproval(ctx, a); ok {
		return err
	}
	id := a.ObjectID
	switch a.Action {
	case "job.delete":
		return s.store.DeleteJob(ctx, id)
	case "job.retention":
		var p repo.RetentionPolicy
		if err := json.Unmarshal(a.Payload, &p); err != nil {
			return err
		}
		j, err := s.store.GetJob(ctx, id)
		if err != nil {
			return err
		}
		j.Retention = p
		return s.store.UpdateJob(ctx, j)
	case "job.release_hold":
		return s.store.releaseRetentionHold(ctx, id)
	case "agent.delete":
		return s.store.DeleteAgent(ctx, id)
	case "vmware.delete":
		return s.store.DeleteVMwareHost(ctx, id)
	case "storage.delete":
		return s.store.DeleteTarget(ctx, id)
	case "user.delete":
		u, err := s.store.GetUser(ctx, id)
		if err != nil {
			return err
		}
		return s.store.deleteUserChecked(ctx, u)
	case "four_eyes.off":
		return s.store.SetSetting(ctx, settingFourEyes, false)
	}
	return fmt.Errorf("unknown action %q", a.Action)
}

// canDecide reports whether user u may approve or reject a.
// Accounts created after the request cannot decide it, so an account made
// for the purpose does not help.
func canDecide(u *User, a Approval) bool {
	return u != nil && u.Username != a.RequestedBy && u.Can(string(approvalActions[a.Action].Perm)) && u.CreatedAt.Before(a.RequestedAt)
}

func (s *Server) handleApprovals(w http.ResponseWriter, r *http.Request, user string) {
	pending, decided, err := s.store.ListApprovals(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, r, "approvals", pageData{Title: "Approvals", Nav: "approvals", User: user, Data: map[string]any{
		"Pending": pending, "Decided": decided, "On": s.store.fourEyes(r.Context())}})
}

// handleApprovalDecision serves POST /approvals/{id}/{approve|reject|cancel}.
func (s *Server) handleApprovalDecision(w http.ResponseWriter, r *http.Request, _ string) {
	ctx := r.Context()
	id, _ := pathID(r)
	s.store.expireApprovals(ctx)
	a, err := s.store.getApproval(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	me := currentUser(r)
	switch r.PathValue("decision") {
	case "cancel":
		if me.Username != a.RequestedBy {
			redirectErr(w, r, "/approvals", errors.New("only the user who requested the change can cancel it"))
			return
		}
		if err := s.store.decideApproval(ctx, id, "cancelled", me.Username, ""); err != nil {
			redirectErr(w, r, "/approvals", err)
			return
		}
		s.audit(r, "approval.cancel", "%s: %s", a.Title(), a.ObjectName)
		redirectMsg(w, r, "/approvals", "Request cancelled.")
	case "reject":
		if !canDecide(me, a) {
			redirectErr(w, r, "/approvals", errors.New("a different user with the right role, whose account existed before the request, must decide"))
			return
		}
		if err := s.store.decideApproval(ctx, id, "rejected", me.Username, ""); err != nil {
			redirectErr(w, r, "/approvals", err)
			return
		}
		s.audit(r, "approval.reject", "%s: %s (requested by %s)", a.Title(), a.ObjectName, a.RequestedBy)
		redirectMsg(w, r, "/approvals", "Request rejected; nothing was changed.")
	case "approve":
		if !canDecide(me, a) {
			redirectErr(w, r, "/approvals", errors.New("a different user with the right role, whose account existed before the request, must decide"))
			return
		}
		// Claim the request first so two approvers cannot run it twice.
		if err := s.store.decideApproval(ctx, id, "approved", me.Username, ""); err != nil {
			redirectErr(w, r, "/approvals", err)
			return
		}
		if err := s.executeApproval(ctx, a); err != nil {
			s.store.db.Exec(ctx, `UPDATE approvals SET status='failed', note=$2 WHERE id=$1`, id, err.Error())
			s.audit(r, "approval.failed", "%s: %s: %v", a.Title(), a.ObjectName, err)
			redirectErr(w, r, "/approvals", fmt.Errorf("approved, but the change failed: %w", err))
			return
		}
		s.audit(r, "approval.approve", "%s: %s (requested by %s)", a.Title(), a.ObjectName, a.RequestedBy)
		redirectMsg(w, r, "/approvals", "Approved and carried out.")
	default:
		http.NotFound(w, r)
	}
}

// handleSettingsFourEyes serves POST /settings/four-eyes. Turning it on
// needs two enabled administrators; turning it off needs approval.
func (s *Server) handleSettingsFourEyes(w http.ResponseWriter, r *http.Request, _ string) {
	ctx := r.Context()
	on := r.FormValue("four_eyes") == "on"
	cur := s.store.fourEyes(ctx)
	switch {
	case on == cur:
		redirectMsg(w, r, "/settings/security", "Settings saved.")
		return
	case on:
		var n int
		if err := s.store.db.QueryRow(ctx, `SELECT count(*) FROM users WHERE role='admin' AND NOT disabled`).Scan(&n); err != nil {
			s.serverError(w, err)
			return
		}
		if n < 2 {
			redirectErr(w, r, "/settings/security", errors.New("four-eyes approval needs at least two enabled administrators"))
			return
		}
	default:
		if s.needsApproval(w, r, "/settings/security", "four_eyes.off", 0, "four-eyes approval", nil) {
			return
		}
	}
	if err := s.store.SetSetting(ctx, settingFourEyes, on); err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, "settings.four_eyes", "on=%v", on)
	redirectMsg(w, r, "/settings/security", "Settings saved.")
}

// shorterRetention reports whether policy cur may delete backups that old
// keeps.
func shorterRetention(old, cur repo.RetentionPolicy) bool {
	switch {
	case cur.Empty():
		return false
	case old.Empty():
		return true
	}
	return cur.KeepLast < old.KeepLast || cur.KeepDaily < old.KeepDaily || cur.KeepWeekly < old.KeepWeekly || cur.KeepMonthly < old.KeepMonthly
}
