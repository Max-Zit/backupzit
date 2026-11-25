package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/backend"
)

// The agent reads storage for the console when the console cannot reach it
// itself (a target marked "Reachable by agents only", 0.61, #63): see
// api.RelayRequest. Only reads are done; the agent never writes or removes
// anything for a relay.

const (
	// relayIdle ends the relay after this long without a request; the
	// console asks again with a poll when it has new reads.
	relayIdle = 10 * time.Minute
	// relayWorkers is how many reads run at once.
	relayWorkers = 8
	// relayReadTimeout bounds one read from the storage and its answer.
	relayReadTimeout = 5 * time.Minute
)

// startRelay starts fetching the console's storage reads, unless that runs
// already.
func (a *Agent) startRelay(ctx context.Context) {
	a.mu.Lock()
	if a.relaying {
		a.mu.Unlock()
		return
	}
	a.relaying = true
	a.mu.Unlock()
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer func() {
			a.mu.Lock()
			a.relaying = false
			a.mu.Unlock()
		}()
		a.relayLoop(ctx)
	}()
}

func (a *Agent) relayLoop(ctx context.Context) {
	a.log.Info("reading storage for the console (browsing a backup)")
	bes := &relayBackends{m: map[string]*relayBackend{}}
	defer bes.closeAll()
	sem := make(chan struct{}, relayWorkers)
	var wg sync.WaitGroup
	defer wg.Wait()
	last, failures := time.Now(), 0
	for ctx.Err() == nil && time.Since(last) < relayIdle {
		var resp api.RelayNextResponse
		if err := a.client.post(ctx, api.PathRelayNext, struct{}{}, &resp, true); err != nil {
			if failures++; failures >= 5 {
				a.log.Warn("reading storage for the console stopped", "err", err)
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		failures = 0
		if len(resp.Requests) > 0 {
			last = time.Now()
		}
		for _, rq := range resp.Requests {
			sem <- struct{}{}
			wg.Add(1)
			go func(rq api.RelayRequest) {
				defer wg.Done()
				defer func() { <-sem }()
				a.relayOne(ctx, bes, rq)
			}(rq)
		}
	}
}

// relayOne does one read and sends the answer.
func (a *Agent) relayOne(ctx context.Context, bes *relayBackends, rq api.RelayRequest) {
	ctx, cancel := context.WithTimeout(ctx, relayReadTimeout)
	defer cancel()
	body, err := relayRead(ctx, bes, rq)
	hdr := map[string]string{}
	switch {
	case errors.Is(err, backend.ErrNotFound):
		hdr[api.RelayNotFoundHeader] = "1"
		body = nil
	case err != nil:
		msg := err.Error()
		if len(msg) > 2000 {
			msg = msg[:2000]
		}
		hdr[api.RelayErrorHeader] = strings.ReplaceAll(strings.ReplaceAll(msg, "\r", " "), "\n", " ")
		body = nil
	}
	if err := a.client.postRaw(ctx, api.PathRelayReplyPrefix+rq.ID, body, hdr); err != nil {
		a.log.Warn("send a storage read to the console", "err", err)
	}
}

// relayRead performs the read of rq.
func relayRead(ctx context.Context, bes *relayBackends, rq api.RelayRequest) ([]byte, error) {
	if !relayName(rq.Name, rq.Op == api.RelayList) {
		return nil, fmt.Errorf("refused object name %q", rq.Name)
	}
	if rq.Op != api.RelayLoad && rq.Op != api.RelayLoadRange && rq.Op != api.RelaySize && rq.Op != api.RelayList {
		return nil, fmt.Errorf("refused operation %q (only reads are relayed)", rq.Op)
	}
	be, err := bes.get(ctx, rq.Repository)
	if err != nil {
		return nil, err
	}
	switch rq.Op {
	case api.RelayLoad:
		return be.Load(ctx, rq.Name)
	case api.RelayLoadRange:
		if rq.Length < 0 || rq.Length > 512<<20 {
			return nil, fmt.Errorf("refused read of %d bytes", rq.Length)
		}
		return be.LoadRange(ctx, rq.Name, rq.Offset, rq.Length)
	case api.RelaySize:
		n, err := be.Size(ctx, rq.Name)
		if err != nil {
			return nil, err
		}
		return []byte(strconv.FormatInt(n, 10)), nil
	default:
		names, err := be.List(ctx, rq.Name)
		if err != nil {
			return nil, err
		}
		if names == nil {
			names = []string{}
		}
		return json.Marshal(names)
	}
}

// relayName accepts object names inside the repository only: relative,
// without "..", backslashes or control characters ("" only for a list of
// the whole repository).
func relayName(name string, list bool) bool {
	if name == "" {
		return list
	}
	if strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\\x00\r\n") {
		return false
	}
	for _, p := range strings.Split(name, "/") {
		if p == ".." {
			return false
		}
	}
	return true
}

// relayBackends keeps the storage opened for relays, one per address and
// credentials, until the relay ends.
type relayBackends struct {
	mu sync.Mutex
	m  map[string]*relayBackend
}

type relayBackend struct {
	once    sync.Once
	be      backend.Backend
	err     error
	cleanup func()
}

func (r *relayBackends) get(ctx context.Context, rs api.Repository) (backend.Backend, error) {
	rs.Password = "" // never needed: the console decrypts
	key, _ := json.Marshal(rs) // #nosec G117 -- only hashed into an in-memory cache key below, never stored or sent
	sum := sha256.Sum256(key)
	k := hex.EncodeToString(sum[:])
	r.mu.Lock()
	e, ok := r.m[k]
	if !ok {
		e = &relayBackend{}
		r.m[k] = e
	}
	r.mu.Unlock()
	e.once.Do(func() {
		opts, cleanup, err := backendOptions(rs)
		if err != nil {
			e.err = err
			return
		}
		be, err := backend.Open(context.WithoutCancel(ctx), rs.URL, opts)
		if err != nil {
			cleanup()
			e.err = err
			return
		}
		e.be, e.cleanup = be, cleanup
	})
	if e.err != nil {
		// Try again with the next read (the storage may be back).
		r.mu.Lock()
		if r.m[k] == e {
			delete(r.m, k)
		}
		r.mu.Unlock()
	}
	return e.be, e.err
}

func (r *relayBackends) closeAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, e := range r.m {
		if e.be != nil {
			_ = e.be.Close()
			e.cleanup()
		}
		delete(r.m, k)
	}
}

// postRaw sends body to the console as it is, with extra headers. It has no
// overall time limit of its own (ctx bounds it), so a large object on a slow
// line is not cut off.
func (c *Client) postRaw(ctx context.Context, path string, body []byte, hdr map[string]string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.ServerURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+c.cfg.AgentUUID+":"+c.cfg.Secret)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	hc := &http.Client{Transport: c.http.Transport}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("server: HTTP %d", resp.StatusCode)
	}
	return nil
}
