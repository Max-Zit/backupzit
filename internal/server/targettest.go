package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/backend"
)

// targetTestTimeout bounds a connection test (tests shorten it).
var targetTestTimeout = 30 * time.Second

// minTargetTestVersion is the first agent that tests storage for the
// console (target-test runs) and opens network storage with the run's
// credentials when it searches it (repo-scan runs).
const minTargetTestVersion = "0.60.0"

// errAgentsOnly: the console does not connect to storage that only the
// agents reach; errAgentsOnlyTest says so for the connection test.
var (
	errAgentsOnly     = errors.New("this storage is reachable by agents only, so the console does not connect to it")
	errAgentsOnlyTest = errors.New("this storage is reachable by agents only, so the console does not connect to it; test it from an agent")
)

// lockNote is what a connection test found about the default Object Lock
// retention of an S3 bucket with immutability on.
type lockNote struct {
	checked  bool
	err      error
	mode     string
	days     int
	lockDays int
}

// text is the warning for the page, "" when the bucket protects its
// objects by itself for at least the target's immutability period.
func (n lockNote) text(l *language) string {
	switch {
	case !n.checked:
		return ""
	case n.err != nil:
		return " " + l.T("The default retention of the bucket could not be checked (%v); make sure the bucket locks new objects in compliance mode for at least %d days by itself.", n.err, n.lockDays)
	case n.mode != "COMPLIANCE":
		return " " + l.T("Warning: the bucket has no default Object Lock retention in compliance mode, so the agents decide which objects are locked. Set a default retention of at least %d days in compliance mode on the bucket, so a compromised computer cannot write unlocked backups.", n.lockDays)
	case n.days < n.lockDays:
		return " " + l.T("Warning: the default retention of the bucket (%d days) is shorter than the immutability period of the target (%d days); set it to at least %d days.", n.days, n.lockDays, n.lockDays)
	}
	return ""
}

// lockNoteOf is the retention note of a test of t that found c.
func lockNoteOf(t Target, c backend.ConnCheck) lockNote {
	if !c.RetentionChecked || t.S3LockDays <= 0 {
		return lockNote{}
	}
	return lockNote{checked: true, err: c.RetentionErr, mode: c.RetentionMode, days: c.RetentionDays, lockDays: t.S3LockDays}
}

// testTargetLock checks that the console can reach a storage target with its
// credentials (backend.CheckConnection, which agents run too): it writes,
// reads back and removes a small file, or only lists on storage where
// files cannot be removed (S3 Object Lock, hardened repository); for an S3
// bucket with immutability it also checks the bucket's default retention.
// Agents write the backups; the console tests from its own network, so it
// refuses storage that only the agents reach.
func testTargetLock(ctx context.Context, t Target) (string, lockNote, error) {
	if localTarget(t) {
		return "", lockNote{}, errors.New("a local or USB disk is only reachable by its agent; the first backup tests it")
	}
	if t.AgentsOnly {
		return "", lockNote{}, errAgentsOnlyTest
	}
	ctx, cancel := context.WithTimeout(ctx, targetTestTimeout)
	defer cancel()
	be, done, err := openTargetBackend(ctx, t, t.URL)
	if err != nil {
		return "", lockNote{}, fmt.Errorf("connect: %w", err)
	}
	defer done()
	c, err := backend.CheckConnection(ctx, be, t.URL, t.S3LockDays)
	if err != nil {
		return "", lockNote{}, err
	}
	return c.Summary(), lockNoteOf(t, c), nil
}

// targetTestMessage words a test result for the page. agent is the computer
// the test ran on, "" for the console: the hints say from where the
// storage must be reachable.
func targetTestMessage(l *language, name, agent, ok string, err error) (string, error) {
	if err != nil {
		hint := ""
		switch m := strings.ToLower(err.Error()); {
		case errors.Is(err, backend.ErrClockSkew) || strings.Contains(m, "clock of this computer differs"):
			// An agent reports the error as text: the hint of the storage
			// itself is in it.
			if agent == "" {
				hint = " " + l.T("(the clock of the console differs from the storage's: turn on time synchronization on the console)")
			} else {
				hint = " " + l.T("(the clock of the agent's computer differs from the storage's: turn on time synchronization there)")
			}
		case strings.Contains(m, "ssh_fx_failure") || strings.Contains(m, "no space") || strings.Contains(m, "quota"):
			hint = " " + l.T("(the storage may be full or over its quota)")
		case strings.Contains(m, "unable to authenticate") || strings.Contains(m, "access denied") || strings.Contains(m, "accessdenied") ||
			strings.Contains(m, "invalidaccesskeyid") || strings.Contains(m, "signaturedoesnotmatch") || strings.Contains(m, "logon failure") ||
			strings.Contains(m, "authenticationfailed") || strings.Contains(m, "401") || strings.Contains(m, "403"):
			hint = " " + l.T("(check the user name, password or key)")
		case strings.Contains(m, "host key") || strings.Contains(m, "fingerprint"):
			hint = " " + l.T("(the server presented another key or certificate than the one entered)")
		case strings.Contains(m, "connection refused") || strings.Contains(m, "no such host") || strings.Contains(m, "timeout") || strings.Contains(m, "deadline exceeded") ||
			strings.Contains(m, "unreachable") || strings.Contains(m, "no route to host"):
			if agent == "" {
				// The usual case of #63: storage on a network only the
				// agents reach.
				hint = " " + l.T("(check the address and the port, and that the console can reach the storage)") + " " +
					l.T("Storage that only the agents reach: test it from an agent and mark it reachable by agents only.")
			} else {
				hint = " " + l.T("(check the address and the port, and that the agent can reach the storage)")
			}
		}
		if agent != "" {
			return "", fmt.Errorf("%s: %s%s", name, l.T("connection test on the agent %s failed: %v", agent, err), hint)
		}
		return "", fmt.Errorf("%s: %s%s", name, l.T("connection test failed: %v", err), hint)
	}
	if agent != "" {
		return name + ": " + l.T("connection test on the agent %s passed — %s", agent, l.T(ok)), nil
	}
	return name + ": " + l.T("connection test passed — %s", l.T(ok)), nil
}

// logTestFailure writes a failed connection test to the console log, so
// it can be found after the page message is gone (#63). agent is the
// computer the test ran on, "" for the console.
func (s *Server) logTestFailure(t Target, agent string, err error) {
	from := "console"
	if agent != "" {
		from = "agent " + agent
	}
	s.log.Warn("storage connection test failed", "target", t.Name, "kind", t.Kind, "from", from, "err", err)
}

// testTarget tests t from the console and words the result for the page; a
// failure is logged.
func (s *Server) testTarget(ctx context.Context, l *language, t Target) (string, error) {
	ok, note, err := testTargetLock(ctx, t)
	if err != nil {
		s.logTestFailure(t, "", err)
	}
	msg, err := targetTestMessage(l, t.Name, "", ok, err)
	if err != nil {
		return "", err
	}
	return msg + note.text(l), nil
}

// QueueTargetTest asks an agent to test the connection to target t (a
// target-test run); it returns the run and the agent.
func (s *Store) QueueTargetTest(ctx context.Context, t Target, agentID int64) (int64, Agent, error) {
	if localTarget(t) {
		return 0, Agent{}, errors.New("a local or USB disk is only reachable by its agent; the first backup tests it")
	}
	a, err := s.GetAgent(ctx, agentID)
	if err != nil || a.RemovedAt != nil || a.Recovery {
		return 0, a, errors.New("choose an agent")
	}
	if agentOlder(a, minTargetTestVersion) {
		return 0, a, textError{"the agent on %s (%s) cannot test storage yet; update it to %s or newer", []any{a.Hostname, a.Version, minTargetTestVersion}}
	}
	var id int64
	// notified: the user waits for the result on the page; it sends no alert.
	err = s.db.QueryRow(ctx, `INSERT INTO runs(agent_id, kind, trigger, repo_url, target_id, notified) VALUES($1,'target-test','manual','',$2,true) RETURNING id`,
		agentID, t.ID).Scan(&id)
	return id, a, err
}

// targetTestRun is the test run of target t, if id names one.
func (s *Store) targetTestRun(ctx context.Context, t Target, id string) (Run, bool) {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return Run{}, false
	}
	run, err := s.GetRun(ctx, n)
	if err != nil || run.Kind != api.KindTargetTest || run.TargetID == nil || *run.TargetID != t.ID {
		return Run{}, false
	}
	return run, true
}

// agentTestResult words the result of a finished test run of t for the
// page, as for a test on the console.
func agentTestResult(l *language, t Target, run Run) (string, error) {
	if run.Status != api.StatusSuccess {
		msg := run.Message
		if msg == "" {
			msg = "the agent sent no result"
		}
		_, err := targetTestMessage(l, t.Name, run.Hostname, "", errors.New(msg))
		return "", err
	}
	var d api.TargetTest
	_ = json.Unmarshal(run.Details, &d)
	c := backend.ConnCheck{Immutable: d.Immutable, RetentionChecked: d.RetentionChecked, RetentionMode: d.RetentionMode, RetentionDays: d.RetentionDays}
	if d.RetentionError != "" {
		c.RetentionErr = errors.New(d.RetentionError)
	}
	msg, _ := targetTestMessage(l, t.Name, run.Hostname, c.Summary(), nil)
	return msg + lockNoteOf(t, c).text(l), nil
}

// finishTargetTest takes the result of a test run from agent a: a failed
// test goes to the console log, as one on the console does. The result
// is shown on the test page; there are no alerts or chained runs.
func (s *Server) finishTargetTest(ctx context.Context, a Agent, run Run, res api.RunResult) {
	if res.Status != api.StatusFailed || run.TargetID == nil {
		return
	}
	t, err := s.store.GetTarget(ctx, *run.TargetID)
	if err != nil {
		return
	}
	s.logTestFailure(t, a.Hostname, errors.New(res.Message))
}

// handleTargetTestPage serves GET /targets/{id}/test: where the test runs
// (the console, or an agent, by default the one that used the storage
// last); while an agent tests, a notice that waits for it; then the result
// on the Storage page, as after a test on the console.
func (s *Server) handleTargetTestPage(w http.ResponseWriter, r *http.Request, user string) {
	id, _ := pathID(r)
	t, err := s.store.GetTarget(r.Context(), id)
	if err != nil || localTarget(t) {
		http.NotFound(w, r)
		return
	}
	data := map[string]any{"Target": t}
	if run, ok := s.store.targetTestRun(r.Context(), t, r.URL.Query().Get("run")); ok {
		if run.Status == api.StatusQueued || run.Status == api.StatusRunning {
			data["Run"] = run
			s.render(w, r, "target_test", pageData{Title: "Test connection", Nav: "targets", User: user, Data: data})
			return
		}
		msg, err := agentTestResult(requestLanguage(r), t, run)
		if err != nil {
			redirectErr(w, r, "/targets", err)
			return
		}
		redirectMsg(w, r, "/targets", msg)
		return
	}
	agents, err := s.store.scanAgents(r.Context(), t)
	if err != nil {
		s.serverError(w, err)
		return
	}
	data["Agents"], data["Last"] = agents, s.store.lastTargetAgent(r.Context(), t)
	s.render(w, r, "target_test", pageData{Title: "Test connection", Nav: "targets", User: user, Data: data})
}

// handleTargetTest serves POST /targets/{id}/test. With from "console" (or
// none, as before 0.60) the console tests at once and the Storage page
// shows the result; with an agent's ID that agent tests in a target-test
// run and the test page waits for it.
func (s *Server) handleTargetTest(w http.ResponseWriter, r *http.Request, _ string) {
	id, _ := pathID(r)
	t, err := s.store.GetTarget(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if from := r.FormValue("from"); from != "" && from != "console" {
		agentID, _ := strconv.ParseInt(from, 10, 64)
		s.startAgentTest(w, r, t, agentID, fmt.Sprintf("/targets/%d/test", t.ID), "")
		return
	}
	if t.AgentsOnly {
		redirectErr(w, r, fmt.Sprintf("/targets/%d/test", t.ID), errAgentsOnlyTest)
		return
	}
	msg, err := s.testTarget(r.Context(), requestLanguage(r), t)
	if err != nil {
		redirectErr(w, r, "/targets", err)
		return
	}
	redirectMsg(w, r, "/targets", msg)
}

// startAgentTest queues a test of t on an agent and sends the browser to
// the page that waits for it, with msg shown there; a refusal goes back
// to back.
func (s *Server) startAgentTest(w http.ResponseWriter, r *http.Request, t Target, agentID int64, back, msg string) {
	l := requestLanguage(r)
	rid, a, err := s.store.QueueTargetTest(r.Context(), t, agentID)
	if err != nil {
		redirectErr(w, r, back, errors.New(strings.TrimSpace(msg+" "+errorIn(l, err))))
		return
	}
	s.audit(r, "storage.test", "connection test of storage %s by the agent on %s, run %d", t.Name, a.Hostname, rid)
	to := fmt.Sprintf("/targets/%d/test?run=%d", t.ID, rid)
	if msg == "" {
		http.Redirect(w, r, to, http.StatusSeeOther)
		return
	}
	redirectMsg(w, r, to, msg)
}

// textError is an error whose text is a catalog text with arguments, so
// the page can show it in the user's language.
type textError struct {
	format string
	args   []any
}

// Error is the English text.
func (e textError) Error() string { return fmt.Sprintf(e.format, e.args...) }

// errorIn is the text of err in l's language: a textError is translated
// with its arguments, another error is translated when its text is a
// catalog text.
func errorIn(l *language, err error) string {
	var te textError
	if errors.As(err, &te) {
		return l.T(te.format, te.args...)
	}
	return l.T(err.Error())
}
