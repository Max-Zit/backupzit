package server

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
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
