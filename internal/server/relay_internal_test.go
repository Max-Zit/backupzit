package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
)

// fakeRelayAgent answers the reads of agent id from be, as an agent does.
func fakeRelayAgent(ctx context.Context, h *relayHub, id int64, be backend.Backend, seen chan<- api.RelayRequest) {
	for ctx.Err() == nil {
		for _, rq := range h.next(ctx, id) {
			if seen != nil {
				seen <- rq
			}
			var res relayResult
			switch rq.Op {
			case api.RelayLoad:
				res.data, res.err = be.Load(ctx, rq.Name)
			case api.RelayLoadRange:
				res.data, res.err = be.LoadRange(ctx, rq.Name, rq.Offset, rq.Length)
			case api.RelaySize:
				var n int64
				n, res.err = be.Size(ctx, rq.Name)
				res.data = []byte(strconv.FormatInt(n, 10))
			case api.RelayList:
				var names []string
				names, res.err = be.List(ctx, rq.Name)
				res.data, _ = json.Marshal(names)
			}
			if errors.Is(res.err, backend.ErrNotFound) {
				res = relayResult{notFound: true}
			}
			h.reply(id, rq.ID, res)
		}
	}
}

// A repository is opened and read through an agent; writes are refused, a
// missing object stays "not found" for the repository code, and the
// repository password never goes to the agent (#63).
func TestRelayBackend(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	be, err := backend.OpenLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be, repo.Password("recovery-key-1"))
	if err != nil {
		t.Fatal(err)
	}
	r.Close()

	var h relayHub
	seen := make(chan api.RelayRequest, 1000)
	go fakeRelayAgent(ctx, &h, 7, be, seen)
	rb := &relayBackend{hub: &h, agentID: 7, agent: "nas-site-pc", repo: api.Repository{URL: dir, SMBPassword: "share-pw"}}
	r2, err := repo.Open(ctx, rb, repo.Password("recovery-key-1"))
	if err != nil {
		t.Fatalf("open through the agent: %v", err)
	}
	r2.Close()
	if names, err := rb.List(ctx, ""); err != nil || len(names) == 0 {
		t.Errorf("list: %v %v", names, err)
	}
	if n, err := rb.Size(ctx, "config"); err != nil || n <= 0 {
		t.Errorf("size: %d %v", n, err)
	}
	if b, err := rb.LoadRange(ctx, "config", 0, 4); err != nil || len(b) != 4 {
		t.Errorf("range: %d %v", len(b), err)
	}
	if _, err := rb.Load(ctx, "data/none"); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("missing object: %v", err)
	}
	if rb.Save(ctx, "x", []byte("y")) == nil || rb.Remove(ctx, "config") == nil {
		t.Error("a write went through")
	}
	close(seen)
	for rq := range seen {
		if rq.Repository.Password != "" || rq.Repository.SMBPassword != "share-pw" {
			t.Fatalf("request carries %+v", rq.Repository)
		}
	}
}

// A read nobody answers ends with an error and leaves nothing queued; an
// answer from another agent is refused.
func TestRelayTimeoutAndWrongAgent(t *testing.T) {
	var h relayHub
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := h.do(ctx, 3, api.RelayRequest{Op: api.RelaySize, Name: "config"}); err == nil {
		t.Fatal("no error without an agent")
	}
	if h.pending(3) || len(h.calls) != 0 {
		t.Fatal("the read stayed queued")
	}

	done := make(chan error, 1)
	go func() {
		_, err := h.do(context.Background(), 4, api.RelayRequest{Op: api.RelaySize, Name: "config"})
		done <- err
	}()
	rq := h.next(context.Background(), 4)
	if len(rq) != 1 {
		t.Fatalf("next: %+v", rq)
	}
	if h.reply(5, rq[0].ID, relayResult{data: []byte("1")}) {
		t.Fatal("another agent answered")
	}
	if !h.reply(4, rq[0].ID, relayResult{data: []byte("12")}) {
		t.Fatal("own answer refused")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// A late second answer finds nothing.
	if h.reply(4, rq[0].ID, relayResult{}) {
		t.Fatal("answered twice")
	}
}

// Storage reachable by agents only is browsed (through an agent) now; local
// disks still are not.
func TestBrowseRefusedAgentsOnly(t *testing.T) {
	if err := browseRefused(Target{Kind: "smb", AgentsOnly: true, SFTPKey: ""}); err != nil {
		t.Errorf("agents only: %v", err)
	}
	if err := browseRefused(Target{Kind: "sftp", AgentsOnly: true, SFTPKey: "KEY"}); err != nil {
		t.Errorf("agents only with a key: %v", err)
	}
	if browseRefused(Target{Kind: "local"}) == nil {
		t.Error("local disk browsed")
	}
}

// countingBody records whether the handler read the answer at all.
type countingBody struct {
	r    *bytes.Reader
	read bool
}

func (b *countingBody) Read(p []byte) (int, error) {
	b.read = true
	return b.r.Read(p)
}

// MED 1: an answer is read only for a read the agent was given, never more
// than was asked for, and only a few large answers at once.
func TestRelayReplyLimits(t *testing.T) {
	s := &Server{}
	post := func(agentID int64, id string, body []byte) (int, *countingBody) {
		cb := &countingBody{r: bytes.NewReader(body)}
		req := httptest.NewRequest("POST", api.PathRelayReplyPrefix+id, cb)
		req.SetPathValue("id", id)
		req = req.WithContext(context.WithValue(req.Context(), agentKey, Agent{ID: agentID, Hostname: "nas-pc"}))
		w := httptest.NewRecorder()
		s.handleRelayReply(w, req)
		return w.Code, cb
	}
	// An unknown ID: 404 before a byte of the answer is read.
	if code, cb := post(7, "no-such-read", bytes.Repeat([]byte{1}, 1<<20)); code != http.StatusNotFound || cb.read {
		t.Fatalf("unknown read: %d, body read %v", code, cb.read)
	}
	ask := func(rq api.RelayRequest) (chan error, chan []byte, string) {
		errc, datac := make(chan error, 1), make(chan []byte, 1)
		go func() {
			b, err := s.relay.do(context.Background(), 7, rq)
			errc <- err
			datac <- b
		}()
		got := s.relay.next(context.Background(), 7)
		if len(got) != 1 {
			t.Fatalf("next: %+v", got)
		}
		return errc, datac, got[0].ID
	}
	// Another agent's answer is refused unread; the own one is taken.
	errc, datac, id := ask(api.RelayRequest{Op: api.RelayLoadRange, Name: "data/ab/abcd", Length: 4})
	if code, cb := post(8, id, []byte("1234")); code != http.StatusNotFound || cb.read {
		t.Fatalf("other agent: %d, body read %v", code, cb.read)
	}
	if code, _ := post(7, id, []byte("1234")); code != http.StatusNoContent {
		t.Fatalf("own answer: %d", code)
	}
	if err, b := <-errc, <-datac; err != nil || string(b) != "1234" {
		t.Fatalf("range: %q %v", b, err)
	}
	// Longer than asked for: refused.
	for _, rq := range []api.RelayRequest{{Op: api.RelayLoadRange, Name: "data/ab/abcd", Length: 4}, {Op: api.RelaySize, Name: "config"}} {
		errc, _, id := ask(rq)
		if code, _ := post(7, id, bytes.Repeat([]byte("9"), 65)); code != http.StatusNoContent {
			t.Fatalf("%s: %d", rq.Op, code)
		}
		if err := <-errc; err == nil || !strings.Contains(err.Error(), "larger than") {
			t.Errorf("%s: an answer larger than asked for was taken: %v", rq.Op, err)
		}
	}
	if answerMax(api.RelayRequest{Op: api.RelayLoad}) != relayMaxBody || answerMax(api.RelayRequest{Op: api.RelayList}) != relayMaxList ||
		answerMax(api.RelayRequest{Op: api.RelayLoadRange, Length: -1}) != relayMaxBody {
		t.Error("answer limits")
	}
	// Only relayBodySlots large answers are read at the same time.
	var releases []func()
	for i := 0; i < relayBodySlots; i++ {
		rel, err := s.relay.bodySlot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, rel)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.relay.bodySlot(ctx); err == nil {
		t.Fatal("more large answers than slots")
	}
	releases[0]()
	if rel, err := s.relay.bodySlot(context.Background()); err != nil {
		t.Fatal(err)
	} else {
		rel()
	}
}
