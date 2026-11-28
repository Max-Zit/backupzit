package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/backend"
)

// Browsing backups on storage only the agents reach (0.61, #63): the console
// reads the repository's objects through an agent that uses the storage
// (api.RelayRequest). The browse code stays the same; only its backend is a
// relayBackend instead of a direct connection.

// minRelayVersion is the first agent that reads storage for the console.
const minRelayVersion = "0.61.0"

const (
	// relayWait is how long the agent's request for work waits.
	relayWait = 25 * time.Second
	// relayCallTimeout bounds one read: the agent learns of it with its
	// next poll (every 30 seconds), then it reads and answers.
	relayCallTimeout = 2 * time.Minute
	// relayMaxBody is the largest answer accepted (the largest pack is
	// 256 MiB).
	relayMaxBody = 300 << 20
	// relayMaxList bounds the answer to a listing (about 800,000 names).
	relayMaxList = 64 << 20
	// relaySmallAnswer: answers up to this size are read at once; larger
	// ones wait for one of relayBodySlots, so only a few large answers are
	// in memory at a time, however many agents send (MED 1).
	relaySmallAnswer = 1 << 20
	relayBodySlots   = 4
)

// relayHub holds the reads waiting for each agent and the ones it is doing.
type relayHub struct {
	mu    sync.Mutex
	queue map[int64][]*relayCall
	calls map[string]*relayCall
	wake  map[int64]chan struct{}
	// bodies limits the large answers read at the same time.
	bodies chan struct{}
}

type relayCall struct {
	agentID int64
	req     api.RelayRequest
	done    chan relayResult // buffered, receives one result
}

type relayResult struct {
	data     []byte
	notFound bool
	err      error
}

func (h *relayHub) init() {
	if h.queue == nil {
		h.queue, h.calls, h.wake = map[int64][]*relayCall{}, map[string]*relayCall{}, map[int64]chan struct{}{}
	}
	if h.bodies == nil {
		h.bodies = make(chan struct{}, relayBodySlots)
	}
}

func (h *relayHub) wakeChan(agentID int64) chan struct{} {
	c, ok := h.wake[agentID]
	if !ok {
		c = make(chan struct{}, 1)
		h.wake[agentID] = c
	}
	return c
}

// pending reports whether reads wait for the agent (told with its poll).
func (h *relayHub) pending(agentID int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.queue[agentID]) > 0
}

// do has the agent perform req and waits for the answer.
func (h *relayHub) do(ctx context.Context, agentID int64, req api.RelayRequest) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, relayCallTimeout)
	defer cancel()
	req.ID = randomID(12)
	c := &relayCall{agentID: agentID, req: req, done: make(chan relayResult, 1)}
	h.mu.Lock()
	h.init()
	h.queue[agentID] = append(h.queue[agentID], c)
	h.calls[req.ID] = c
	select {
	case h.wakeChan(agentID) <- struct{}{}:
	default:
	}
	h.mu.Unlock()
	select {
	case res := <-c.done:
		if res.notFound {
			return nil, fmt.Errorf("%s: %w", req.Name, backend.ErrNotFound)
		}
		return res.data, res.err
	case <-ctx.Done():
		h.mu.Lock()
		delete(h.calls, req.ID)
		q := h.queue[agentID]
		for i, x := range q {
			if x == c {
				h.queue[agentID] = append(q[:i:i], q[i+1:]...)
				break
			}
		}
		h.mu.Unlock()
		return nil, errors.New("the agent did not answer in time (it reads the storage for the console; it must be online and version 0.61 or newer)")
	}
}

// next hands the waiting reads to the agent, waiting up to relayWait for
// some to arrive.
func (h *relayHub) next(ctx context.Context, agentID int64) []api.RelayRequest {
	deadline := time.NewTimer(relayWait)
	defer deadline.Stop()
	for {
		h.mu.Lock()
		h.init()
		q := h.queue[agentID]
		if len(q) > 0 {
			n := min(len(q), 16)
			out := make([]api.RelayRequest, 0, n)
			for _, c := range q[:n] {
				out = append(out, c.req)
			}
			h.queue[agentID] = q[n:]
			h.mu.Unlock()
			return out
		}
		wake := h.wakeChan(agentID)
		h.mu.Unlock()
		select {
		case <-wake:
		case <-deadline.C:
			return nil
		case <-ctx.Done():
			return nil
		}
	}
}

// claim takes the read id of agentID for its answer, so an answer is read
// only for a read this agent was given, and only once (MED 1: before, any
// agent could have the console read 300 MiB for an ID that did not exist).
func (h *relayHub) claim(agentID int64, id string) (*relayCall, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.calls[id]
	if !ok || c.agentID != agentID {
		return nil, false
	}
	delete(h.calls, id)
	return c, true
}

// reply delivers the agent's answer to a read it was given.
func (h *relayHub) reply(agentID int64, id string, res relayResult) bool {
	c, ok := h.claim(agentID, id)
	if !ok {
		return false
	}
	c.done <- res // buffered: never blocks, also when the reader gave up
	return true
}

// bodySlot waits for one of the slots for reading a large answer and
// returns its release.
func (h *relayHub) bodySlot(ctx context.Context) (func(), error) {
	h.mu.Lock()
	h.init()
	slots := h.bodies
	h.mu.Unlock()
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// answerMax is the largest answer the console accepts for rq: what it asked
// for, not more (MED 1).
func answerMax(rq api.RelayRequest) int64 {
	switch rq.Op {
	case api.RelaySize:
		return 64 // a decimal number
	case api.RelayList:
		return relayMaxList
	case api.RelayLoadRange:
		if rq.Length >= 0 && rq.Length <= relayMaxBody {
			return int64(rq.Length)
		}
	}
	return relayMaxBody
}

// handleRelayNext serves POST /api/agent/relay/next (agent).
func (s *Server) handleRelayNext(w http.ResponseWriter, r *http.Request) {
	a := agentFrom(r)
	reqs := s.relay.next(r.Context(), a.ID)
	if reqs == nil {
		reqs = []api.RelayRequest{}
	}
	writeJSON(w, http.StatusOK, api.RelayNextResponse{Requests: reqs})
}

// handleRelayReply serves POST /api/agent/relay/reply/{id} (agent). The read
// is looked up before anything of the answer is read: an unknown ID, or one
// given to another agent, gets 404 at once.
func (s *Server) handleRelayReply(w http.ResponseWriter, r *http.Request) {
	a := agentFrom(r)
	c, ok := s.relay.claim(a.ID, r.PathValue("id"))
	if !ok {
		writeAPIError(w, http.StatusNotFound, "no such read (it timed out)")
		return
	}
	res := relayResult{}
	switch {
	case r.Header.Get(api.RelayNotFoundHeader) == "1":
		res.notFound = true
	case r.Header.Get(api.RelayErrorHeader) != "":
		res.err = fmt.Errorf("agent %s: %s", a.Name(), clip(r.Header.Get(api.RelayErrorHeader), 2000))
	default:
		res = s.readRelayAnswer(r, a, answerMax(c.req))
	}
	c.done <- res // buffered: never blocks, also when the reader gave up
	w.WriteHeader(http.StatusNoContent)
}

// readRelayAnswer reads an answer of at most limit bytes.
func (s *Server) readRelayAnswer(r *http.Request, a Agent, limit int64) relayResult {
	if limit > relaySmallAnswer {
		release, err := s.relay.bodySlot(r.Context())
		if err != nil {
			return relayResult{err: fmt.Errorf("agent %s: %w", a.Name(), err)}
		}
		defer release()
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	switch {
	case err != nil:
		return relayResult{err: fmt.Errorf("agent %s: %w", a.Name(), err)}
	case int64(len(b)) > limit:
		return relayResult{err: fmt.Errorf("agent %s: the answer is larger than the %d bytes asked for", a.Name(), limit)}
	}
	return relayResult{data: b}
}

// relayBackend reads a repository through an agent. It is read-only.
type relayBackend struct {
	hub     *relayHub
	agentID int64
	agent   string
	repo    api.Repository // storage and credentials, no repository password
}

var errRelayReadOnly = errors.New("storage read through an agent is read-only")

func (b *relayBackend) call(ctx context.Context, op, name string, off int64, n int) ([]byte, error) {
	return b.hub.do(ctx, b.agentID, api.RelayRequest{Repository: b.repo, Op: op, Name: name, Offset: off, Length: n})
}

func (b *relayBackend) Save(context.Context, string, []byte) error { return errRelayReadOnly }
func (b *relayBackend) Remove(context.Context, string) error       { return errRelayReadOnly }
func (b *relayBackend) Close() error                               { return nil }
func (b *relayBackend) Location() string                           { return b.repo.URL + " (through agent " + b.agent + ")" }

func (b *relayBackend) Load(ctx context.Context, name string) ([]byte, error) {
	return b.call(ctx, api.RelayLoad, name, 0, 0)
}

func (b *relayBackend) LoadRange(ctx context.Context, name string, offset int64, length int) ([]byte, error) {
	return b.call(ctx, api.RelayLoadRange, name, offset, length)
}

func (b *relayBackend) Size(ctx context.Context, name string) (int64, error) {
	d, err := b.call(ctx, api.RelaySize, name, 0, 0)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(string(d), 10, 64)
}

func (b *relayBackend) List(ctx context.Context, dir string) ([]string, error) {
	d, err := b.call(ctx, api.RelayList, dir, 0, 0)
	if err != nil {
		return nil, err
	}
	var names []string
	if err := json.Unmarshal(d, &names); err != nil {
		return nil, fmt.Errorf("list through agent %s: %w", b.agent, err)
	}
	return names, nil
}

type relayAgentKey struct{}

// withRelayAgent names the agent preferred for reading a backup's storage:
// the one that made it.
func withRelayAgent(ctx context.Context, agentID int64) context.Context {
	return context.WithValue(ctx, relayAgentKey{}, agentID)
}

// relayOpen opens storage t through an agent: the one that made the backup
// when it is online, else another online agent that has used the storage.
func (s *Server) relayOpen(ctx context.Context, t Target, url string) (backend.Backend, error) {
	preferred, _ := ctx.Value(relayAgentKey{}).(int64)
	a, err := s.relayAgent(ctx, t, preferred)
	if err != nil {
		return nil, err
	}
	rs := targetRepository(t, url)
	rs.Password = "" // the console decrypts; the agent does not need it
	return &relayBackend{hub: &s.relay, agentID: a.ID, agent: a.Name(), repo: rs}, nil
}

// errNoRelayAgent: no agent that can read the storage for the console is
// online.
var errNoRelayAgent = errors.New("this backup is on storage reachable by agents only, and no agent that uses this storage is online to read it for the console; start one of them, or restore with the paths instead")

// relayAgent picks the agent that reads storage t for the console: the
// preferred one when it is online and new enough, else the online agent that
// used t most recently (#63: several agents of one site share a NAS).
func (s *Server) relayAgent(ctx context.Context, t Target, preferred int64) (Agent, error) {
	ids, err := s.store.targetAgents(ctx, t.ID)
	if err != nil {
		return Agent{}, err
	}
	if preferred != 0 {
		ids = append([]int64{preferred}, ids...)
	}
	agents, err := s.store.ListAgents(ctx)
	if err != nil {
		return Agent{}, err
	}
	byID := map[int64]Agent{}
	for _, a := range agents {
		byID[a.ID] = a
	}
	var old []string
	for _, id := range ids {
		a, ok := byID[id]
		if !ok || a.Recovery || !a.Online() {
			continue
		}
		if agentOlder(a, minRelayVersion) {
			old = append(old, a.Name())
			continue
		}
		return a, nil
	}
	if len(old) > 0 {
		return Agent{}, fmt.Errorf("this backup is on storage reachable by agents only; the console reads it through an agent of version %s or newer: update %s", minRelayVersion, joinNames(old))
	}
	return Agent{}, errNoRelayAgent
}

func joinNames(n []string) string {
	out := ""
	for i, x := range n {
		if i > 0 {
			out += ", "
		}
		out += x
	}
	return out
}

// targetAgents lists the agents that ran something on storage target id,
// the most recent first.
func (s *Store) targetAgents(ctx context.Context, id int64) ([]int64, error) {
	rows, err := s.db.Query(ctx, `SELECT agent_id FROM runs WHERE target_id=$1 AND agent_id IS NOT NULL
		GROUP BY agent_id ORDER BY max(queued_at) DESC LIMIT 50`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var a int64
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
