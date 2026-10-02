// Package localipc is the local channel between the agent service and the
// tray app (and "backupzit-agent status"): a named pipe on Windows, a Unix
// socket elsewhere. Each connection carries one JSON request and one JSON
// response. It is kept free of heavy dependencies so the tray app stays small.
package localipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"time"
)

// Request commands.
const (
	CmdStatus = "status"
	CmdRun    = "run" // start JobID now
)

type Request struct {
	Cmd   string `json:"cmd"`
	JobID int64  `json:"job_id,omitempty"`
}

type Response struct {
	OK     bool    `json:"ok"`
	Error  string  `json:"error,omitempty"`
	Status *Status `json:"status,omitempty"`
}

// Status is what the tray app shows.
type Status struct {
	Version     string     `json:"version"`
	Hostname    string     `json:"hostname"`
	ServerURL   string     `json:"server_url"`
	Enrolled    bool       `json:"enrolled"`
	Connected   bool       `json:"connected"`
	LastContact *time.Time `json:"last_contact,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	Current     *Run       `json:"current,omitempty"`
	Last        *Run       `json:"last,omitempty"`
	// Jobs come from the console; JobsAt is when they were received.
	Jobs   []Job      `json:"jobs"`
	JobsAt *time.Time `json:"jobs_at,omitempty"`
}

// Run is a run executed by the agent.
type Run struct {
	ID       int64      `json:"id"`
	Kind     string     `json:"kind"`
	Job      string     `json:"job,omitempty"`
	Started  time.Time  `json:"started"`
	Finished *time.Time `json:"finished,omitempty"`
	Status   string     `json:"status,omitempty"`
	Message  string     `json:"message,omitempty"`
	Files    uint64     `json:"files,omitempty"`
	Done     uint64     `json:"done,omitempty"`
	Total    uint64     `json:"total,omitempty"`
}

// Job is a backup job of this machine.
type Job struct {
	ID           int64      `json:"id"`
	Name         string     `json:"name"`
	Kind         string     `json:"kind"`
	Enabled      bool       `json:"enabled"`
	Schedule     string     `json:"schedule"`
	NextRun      *time.Time `json:"next_run,omitempty"`
	LastStatus   string     `json:"last_status,omitempty"`
	LastFinished *time.Time `json:"last_finished,omitempty"`
	LastMessage  string     `json:"last_message,omitempty"`
	Running      bool       `json:"running,omitempty"`
}

// Call sends one request to the agent service.
func Call(ctx context.Context, req Request) (*Response, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	c, err := dial(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	}
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return nil, err
	}
	var resp Response
	if err := json.NewDecoder(bufio.NewReader(c)).Decode(&resp); err != nil {
		return nil, err
	}
	if !resp.OK && resp.Error != "" {
		return &resp, errors.New(resp.Error)
	}
	return &resp, nil
}

// Serve answers requests on l until ctx ends.
func Serve(ctx context.Context, l net.Listener, handle func(context.Context, Request) Response) {
	go func() { <-ctx.Done(); l.Close() }()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			time.Sleep(200 * time.Millisecond)
			continue
		}
		go func(c net.Conn) {
			defer c.Close()
			c.SetDeadline(time.Now().Add(30 * time.Second))
			var req Request
			if err := json.NewDecoder(bufio.NewReader(c)).Decode(&req); err != nil {
				return
			}
			resp := handle(ctx, req)
			json.NewEncoder(c).Encode(resp)
		}(c)
	}
}
