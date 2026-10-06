package main

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/kardianos/service"
	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/localipc"
)

// waitingStatus answers the tray app while the agent is not enrolled.
type waitingStatus struct {
	mu     sync.Mutex
	st     localipc.Status
	cancel context.CancelFunc
	done   chan struct{}
}

func startWaitingStatus(ctx context.Context, log *slog.Logger) *waitingStatus {
	host, _ := os.Hostname()
	w := &waitingStatus{st: localipc.Status{Version: version, Hostname: host, Jobs: []localipc.Job{}}, done: make(chan struct{})}
	wctx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	l, err := localipc.Listen()
	if err != nil {
		log.Warn("local status channel unavailable", "err", err)
		close(w.done)
		return w
	}
	go func() {
		defer close(w.done)
		localipc.Serve(wctx, l, func(context.Context, localipc.Request) localipc.Response {
			w.mu.Lock()
			defer w.mu.Unlock()
			st := w.st
			return localipc.Response{OK: true, Status: &st}
		})
	}()
	if !service.Interactive() {
		agent.StartTrayInSessions(log)
	}
	return w
}

// set records the console being tried and the last error ("" = none).
func (w *waitingStatus) set(server, lastErr string) {
	w.mu.Lock()
	w.st.ServerURL, w.st.LastError = server, lastErr
	w.mu.Unlock()
}

// stop frees the status channel for the enrolled agent.
func (w *waitingStatus) stop() {
	w.cancel()
	select {
	case <-w.done:
	case <-time.After(2 * time.Second):
	}
}
