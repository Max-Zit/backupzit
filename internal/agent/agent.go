// Package agent connects a machine to the management server: it enrolls,
// polls for work and executes backup and restore runs.
package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/backupzit/backupzit/internal/api"
	"github.com/backupzit/backupzit/internal/archiver"
	"github.com/backupzit/backupzit/internal/backend"
	"github.com/backupzit/backupzit/internal/repo"
	"github.com/backupzit/backupzit/internal/restorer"
)

// Config is persisted after enrollment.
type Config struct {
	ServerURL   string `json:"server_url"`
	Fingerprint string `json:"server_fingerprint"`
	AgentUUID   string `json:"agent_uuid"`
	Secret      string `json:"secret"`
}

// DefaultConfigPath is where the service keeps its configuration.
func DefaultConfigPath() string {
	if runtime.GOOS == "windows" {
		pd := os.Getenv("ProgramData")
		if pd == "" {
			pd = `C:\ProgramData`
		}
		return filepath.Join(pd, "backupzit", "agent.json")
	}
	return "/etc/backupzit/agent.json"
}

func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.ServerURL == "" || c.AgentUUID == "" || c.Secret == "" {
		return nil, fmt.Errorf("%s: incomplete configuration, enroll the agent again", path)
	}
	return &c, nil
}

func (c *Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// httpClient pins the server certificate by SHA-256 fingerprint. The
// server uses a self-signed certificate, so normal CA validation is
// replaced by the pin obtained at enrollment.
func httpClient(fingerprint string) *http.Client {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: true, // verified by VerifyConnection below
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 {
					return errors.New("server presented no certificate")
				}
				h := sha256.Sum256(cs.PeerCertificates[0].Raw)
				got := "SHA256:" + base64.RawStdEncoding.EncodeToString(h[:])
				if got != fingerprint {
					return fmt.Errorf("server certificate fingerprint mismatch: expected %s, got %s", fingerprint, got)
				}
				return nil
			},
		},
		Proxy:               http.ProxyFromEnvironment,
		TLSHandshakeTimeout: 15 * time.Second,
	}
	return &http.Client{Transport: tr, Timeout: 60 * time.Second}
}

func hostInfo(version string) (hostname, osName, arch string) {
	hostname, _ = os.Hostname()
	return hostname, osDescription(), runtime.GOARCH
}

// Enroll registers this machine with the server and returns the config to save.
func Enroll(ctx context.Context, serverURL, token, fingerprint, version string) (*Config, error) {
	return enroll(ctx, serverURL, token, fingerprint, version, false)
}

// EnrollRecovery registers a temporary agent running from recovery media.
func EnrollRecovery(ctx context.Context, serverURL, token, fingerprint, version string) (*Config, error) {
	return enroll(ctx, serverURL, token, fingerprint, version, true)
}

func enroll(ctx context.Context, serverURL, token, fingerprint, version string, recovery bool) (*Config, error) {
	serverURL = strings.TrimRight(serverURL, "/")
	if !strings.HasPrefix(serverURL, "https://") {
		return nil, errors.New("server URL must start with https://")
	}
	if !strings.HasPrefix(fingerprint, "SHA256:") {
		return nil, errors.New("server fingerprint must look like SHA256:...")
	}
	host, osName, arch := hostInfo(version)
	c := &Client{cfg: &Config{ServerURL: serverURL, Fingerprint: fingerprint}, http: httpClient(fingerprint)}
	var resp api.EnrollResponse
	err := c.post(ctx, api.PathEnroll, api.EnrollRequest{Token: token, Hostname: host, OS: osName, Arch: arch, Version: version, Recovery: recovery}, &resp, false)
	if err != nil {
		return nil, err
	}
	return &Config{ServerURL: serverURL, Fingerprint: fingerprint, AgentUUID: resp.AgentUUID, Secret: resp.Secret}, nil
}

// Client talks to the management server.
type Client struct {
	cfg  *Config
	http *http.Client
}

func NewClient(cfg *Config) *Client {
	return &Client{cfg: cfg, http: httpClient(cfg.Fingerprint)}
}

func (c *Client) post(ctx context.Context, path string, in, out any, auth bool) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.ServerURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if auth {
		req.Header.Set("Authorization", "Bearer "+c.cfg.AgentUUID+":"+c.cfg.Secret)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		var e api.Error
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return fmt.Errorf("server: %s", e.Error)
		}
		return fmt.Errorf("server: HTTP %d", resp.StatusCode)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

// Agent runs the poll loop.
type Agent struct {
	client  *Client
	log     *slog.Logger
	version string
	// VSS makes Windows backups read from shadow copies (default on Windows).
	VSS bool
	// InventoryInterval is how often disks are reported (default 15 min).
	InventoryInterval time.Duration

	mu   sync.Mutex
	busy bool
	// inventoryAt is when the disk inventory was last sent.
	inventoryAt time.Time
	wg          sync.WaitGroup
}

func New(cfg *Config, log *slog.Logger, version string) *Agent {
	return &Agent{client: NewClient(cfg), log: log, version: version, VSS: runtime.GOOS == "windows"}
}

// PollOnce sends one heartbeat and starts a run if one was assigned.
// It returns the server's requested poll interval.
func (a *Agent) PollOnce(ctx context.Context) (time.Duration, error) {
	a.mu.Lock()
	busy := a.busy
	invAt := a.inventoryAt
	a.mu.Unlock()
	host, osName, arch := hostInfo(a.version)
	var resp api.PollResponse
	req := api.PollRequest{Hostname: host, OS: osName, Arch: arch, Version: a.version, Busy: busy}
	invEvery := a.InventoryInterval
	if invEvery <= 0 {
		invEvery = inventoryInterval
	}
	sendInventory := !busy && time.Since(invAt) > invEvery
	if sendInventory {
		req.Disks = a.diskInventory()
	}
	err := a.client.post(ctx, api.PathPoll, req, &resp, true)
	if err != nil {
		return 30 * time.Second, err
	}
	if sendInventory {
		a.mu.Lock()
		a.inventoryAt = time.Now()
		a.mu.Unlock()
	}
	if resp.Run != nil {
		a.mu.Lock()
		a.busy = true
		a.mu.Unlock()
		a.wg.Add(1)
		go func(run api.Run) {
			defer a.wg.Done()
			a.execute(ctx, run)
			a.mu.Lock()
			a.busy = false
			a.mu.Unlock()
		}(*resp.Run)
	}
	iv := time.Duration(resp.PollIntervalSec) * time.Second
	if iv <= 0 {
		iv = 30 * time.Second
	}
	return iv, nil
}

// Wait blocks until running work has finished.
func (a *Agent) Wait() { a.wg.Wait() }

// Run polls until ctx is cancelled. While a run is in progress, polling
// continues as a heartbeat.
func (a *Agent) Run(ctx context.Context) {
	a.log.Info("agent started", "server", a.client.cfg.ServerURL, "version", a.version)
	for {
		iv, err := a.PollOnce(ctx)
		if err != nil && ctx.Err() == nil {
			a.log.Warn("poll failed", "err", err)
		}
		a.mu.Lock()
		if a.busy && iv > 15*time.Second {
			iv = 15 * time.Second
		}
		a.mu.Unlock()
		select {
		case <-ctx.Done():
			a.Wait()
			return
		case <-time.After(iv):
		}
	}
}

func (a *Agent) execute(ctx context.Context, run api.Run) {
	a.log.Info("run started", "run", run.ID, "kind", run.Kind, "job", run.JobName)
	var res api.RunResult
	switch run.Kind {
	case api.KindBackup:
		res = a.backup(ctx, run)
	case api.KindRestore:
		res = a.restore(ctx, run)
	case api.KindImageBackup:
		res = a.imageBackup(ctx, run)
	case api.KindImageFileRestore:
		res = a.imageFileRestore(ctx, run)
	case api.KindImageRestore:
		res = a.imageRestore(ctx, run)
		a.mu.Lock()
		a.inventoryAt = time.Time{} // disk layout changed
		a.mu.Unlock()
	default:
		res = api.RunResult{Status: api.StatusFailed, Message: "unsupported run kind " + run.Kind}
	}
	a.log.Info("run finished", "run", run.ID, "status", res.Status, "message", res.Message, "errors", len(res.Errors))
	// Report with a fresh context: the result must reach the server even
	// when the agent is shutting down. Retry for a while on network errors.
	for i := 0; i < 20; i++ {
		rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := a.client.post(rctx, fmt.Sprintf("%s%d/finish", api.PathRunsPrefix, run.ID), res, nil, true)
		cancel()
		if err == nil {
			return
		}
		a.log.Warn("report run result", "run", run.ID, "err", err)
		if strings.Contains(err.Error(), "not found or not running") {
			return
		}
		time.Sleep(time.Duration(i+1) * 3 * time.Second)
	}
}

func (a *Agent) openRepo(ctx context.Context, rs api.Repository, create bool) (*repo.Repository, func(), error) {
	opts := backend.Options{SFTPPassword: rs.SFTPPassword, SFTPHostKey: rs.SFTPHostKey}
	var cleanup = func() {}
	if rs.SFTPKey != "" {
		f, err := os.CreateTemp("", "bz-key-*")
		if err != nil {
			return nil, nil, err
		}
		f.Write([]byte(rs.SFTPKey))
		f.Close()
		opts.SFTPKeyFile = f.Name()
		cleanup = func() { os.Remove(f.Name()) }
	}
	be, err := backend.Open(ctx, rs.URL, opts)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	r, err := repo.Open(ctx, be)
	if errors.Is(err, repo.ErrNotInitialized) && create {
		r, err = repo.Init(ctx, be)
	}
	if err != nil {
		be.Close()
		cleanup()
		return nil, nil, err
	}
	return r, func() { r.Close(); cleanup() }, nil
}

func failed(err error) api.RunResult {
	return api.RunResult{Status: api.StatusFailed, Message: err.Error()}
}

func (a *Agent) backup(ctx context.Context, run api.Run) api.RunResult {
	r, closeRepo, err := a.openRepo(ctx, run.Repository, true)
	if err != nil {
		return failed(fmt.Errorf("open repository: %w", err))
	}
	defer closeRepo()
	lock, err := r.Lock(ctx, false, lockWait)
	if err != nil {
		return failed(err)
	}
	defer lock.Unlock()
	sn, err := archiver.Run(ctx, r, archiver.Options{
		Paths:    run.Paths,
		Excludes: run.Excludes,
		Version:  a.version,
		Tags:     []string{fmt.Sprintf("run:%d", run.ID), jobTag(run.JobID)},
		VSS:      a.VSS,
	})
	if err != nil {
		return failed(err)
	}
	stats, _ := json.Marshal(sn.Stats)
	res := api.RunResult{Status: api.StatusSuccess, SnapshotID: sn.ID.String(), Stats: stats, Errors: sn.Stats.Errors}
	if len(sn.VSSVolumes) > 0 {
		res.Message = "Read from VSS snapshot of " + strings.Join(sn.VSSVolumes, ", ")
	}
	if len(sn.Stats.Errors) > 0 {
		res.Status = api.StatusWarning
		res.Message = strings.TrimSpace(fmt.Sprintf("%d files or folders could not be read. %s", len(sn.Stats.Errors), res.Message))
	}
	lock.Unlock() // retention needs an exclusive lock
	return a.withRetention(ctx, r, run, res)
}

func (a *Agent) restore(ctx context.Context, run api.Run) api.RunResult {
	r, closeRepo, err := a.openRepo(ctx, run.Repository, false)
	if err != nil {
		return failed(fmt.Errorf("open repository: %w", err))
	}
	defer closeRepo()
	lock, err := r.Lock(ctx, false, lockWait)
	if err != nil {
		return failed(err)
	}
	defer lock.Unlock()
	sn, err := r.LoadSnapshot(ctx, run.SnapshotID)
	if err != nil {
		return failed(err)
	}
	st, err := restorer.Run(ctx, r, sn, restorer.Options{
		Target:  run.RestoreTarget,
		Include: run.Includes,
		Verify:  run.Verify,
	})
	if err != nil {
		return failed(err)
	}
	stats, _ := json.Marshal(st)
	res := api.RunResult{Status: api.StatusSuccess, Stats: stats, Errors: st.Errors}
	if len(st.Errors) > 0 {
		res.Status = api.StatusWarning
		res.Message = fmt.Sprintf("%d items could not be restored", len(st.Errors))
	}
	return res
}

// FetchFingerprint connects to serverURL and returns the fingerprint of the
// certificate it presents, for trust-on-first-use confirmation by a human.
func FetchFingerprint(ctx context.Context, serverURL string) (string, error) {
	u := strings.TrimPrefix(strings.TrimRight(serverURL, "/"), "https://")
	if !strings.Contains(u, ":") {
		u += ":443"
	}
	d := tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true}} // fingerprint is shown to the operator
	conn, err := d.DialContext(ctx, "tcp", u)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return "", errors.New("server presented no certificate")
	}
	h := sha256.Sum256(certs[0].Raw)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(h[:]), nil
}
