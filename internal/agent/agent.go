// Package agent connects a machine to the management server: it enrolls,
// polls for work and executes backup and restore runs.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/archiver"
	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/restorer"
	"github.com/max-zit/backupzit/internal/tlsutil"
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
	if path == DefaultConfigPath() {
		if err := protectDir(filepath.Dir(path)); err != nil {
			return fmt.Errorf("protect configuration directory: %w", err)
		}
	}
	defer func() {
		if path == DefaultConfigPath() {
			MarkEnrolled()
		}
	}()
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
		TLSClientConfig:     tlsutil.PinnedConfig(fingerprint),
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

	localOnce sync.Once
	ls        *localState
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
	req := api.PollRequest{Hostname: host, OS: osName, Arch: arch, Version: a.version, Busy: busy, IPs: localIPs()}
	req.WantStatus = a.wantStatus()
	invEvery := a.InventoryInterval
	if invEvery <= 0 {
		invEvery = inventoryInterval
	}
	sendInventory := !busy && time.Since(invAt) > invEvery
	if sendInventory {
		req.Disks = a.diskInventory()
		req.Hypervisor = a.hypervisorInventory(ctx)
	}
	err := a.client.post(ctx, api.PathPoll, req, &resp, true)
	a.recordPoll(err, resp.Status)
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
		case <-a.local().pollNow:
		}
	}
}

func (a *Agent) execute(ctx context.Context, run api.Run) {
	a.log.Info("run started", "run", run.ID, "kind", run.Kind, "job", run.JobName)
	a.runStarted(run)
	var res api.RunResult
	var after func() // runs once the result is reported
	if run.Limit != nil {
		limit := run.Limit
		ctx = backend.WithLimiter(ctx, backend.NewLimiter(func() int64 {
			if limit.Active(time.Now().Hour()) {
				return limit.BytesPerSec
			}
			return 0
		}))
	}
	kind := run.Kind
	if run.PreCommand != "" && backupKind(run.Kind) {
		if out, err := a.jobCommand(ctx, run, "pre", run.PreCommand, ""); err != nil {
			res = api.RunResult{Status: api.StatusFailed, Message: "The command before the backup failed (" + err.Error() + "); the backup did not run", Errors: commandOutput(out)}
			kind = "" // skip the backup
		}
	}
	switch kind {
	case "":
		// The command before the backup failed.
	case api.KindBackup:
		res = a.backup(ctx, run)
	case api.KindRestore:
		res = a.restore(ctx, run)
	case api.KindImageBackup:
		res = a.imageBackup(ctx, run)
	case api.KindImageFileRestore:
		res = a.imageFileRestore(ctx, run)
	case api.KindCopy:
		res = a.copyRun(ctx, run)
	case api.KindVerify:
		res = a.verifyRun(ctx, run)
	case api.KindSystemBackup:
		res = a.systemBackup(ctx, run)
	case api.KindSystemRestore:
		res = a.systemRestore(ctx, run)
	case api.KindVMFileRestore:
		res = a.vmFileRestore(ctx, run)
	case api.KindAgentUpdate:
		res, after = a.selfUpdate(ctx, run)
	case api.KindVMBackup:
		res = a.vmBackup(ctx, run)
	case api.KindVMRestore:
		res = a.vmRestore(ctx, run)
		a.mu.Lock()
		a.inventoryAt = time.Time{} // guest list changed
		a.mu.Unlock()
	case api.KindSQLBackup, api.KindSQLLog:
		res = a.sqlBackup(ctx, run)
	case api.KindSQLRestore:
		res = a.sqlRestore(ctx, run)
	case api.KindVMReplica, api.KindVMReplicaStart:
		res = a.vmReplica(ctx, run)
		a.mu.Lock()
		a.inventoryAt = time.Time{}
		a.mu.Unlock()
	case api.KindVMInstant, api.KindVMInstantFinish, api.KindVMInstantDiscard:
		res = a.vmInstant(ctx, run)
		a.mu.Lock()
		a.inventoryAt = time.Time{}
		a.mu.Unlock()
	case api.KindImageRestore:
		res = a.imageRestore(ctx, run)
		a.mu.Lock()
		a.inventoryAt = time.Time{} // disk layout changed
		a.mu.Unlock()
	default:
		res = api.RunResult{Status: api.StatusFailed, Message: "unsupported run kind " + run.Kind}
	}
	if run.PostCommand != "" && backupKind(run.Kind) {
		// Runs also after failures, e.g. to restart services the command
		// before the backup stopped.
		if out, err := a.jobCommand(ctx, run, "post", run.PostCommand, res.Status); err != nil {
			if res.Status == api.StatusSuccess {
				res.Status = api.StatusWarning
			}
			res.Message = strings.TrimSpace(res.Message + ". The command after the backup failed (" + err.Error() + ")")
			res.Errors = append(res.Errors, commandOutput(out)...)
		}
	}
	if strings.HasPrefix(run.Repository.URL, "usb://") && res.Status != api.StatusFailed && (run.Kind == api.KindBackup || run.Kind == api.KindImageBackup || run.Kind == api.KindVMBackup || run.Kind == api.KindSystemBackup || run.Kind == api.KindCopy) {
		// Record which of the rotating disks holds this backup.
		if _, loc, err := backend.ResolveUSB(run.Repository.URL); err == nil {
			res.RepoURL = loc
			res.Message = strings.TrimSpace("Disk " + backend.USBLabel(loc) + ". " + res.Message)
		}
	}
	a.log.Info("run finished", "run", run.ID, "status", res.Status, "message", res.Message, "errors", len(res.Errors))
	a.runFinished(res)
	// Report with a fresh context: the result must reach the server even
	// when the agent is shutting down. Retry for a while on network errors.
	for i := 0; i < 20; i++ {
		rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := a.client.post(rctx, fmt.Sprintf("%s%d/finish", api.PathRunsPrefix, run.ID), res, nil, true)
		cancel()
		if err == nil {
			if after != nil {
				after()
			}
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
	opts := backend.Options{SFTPPassword: rs.SFTPPassword, SFTPHostKey: rs.SFTPHostKey,
		S3AccessKey: rs.S3AccessKey, S3SecretKey: rs.S3SecretKey, S3Region: rs.S3Region, S3LockDays: rs.S3LockDays,
		SMBPassword: rs.SMBPassword, SMBDomain: rs.SMBDomain,
		HardenedKey: rs.HardenedKey, HardenedFingerprint: rs.HardenedFingerprint,
		AzureKey: rs.AzureKey, AzureSAS: rs.AzureSAS}
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
	// Uploads honor the speed limit of the run (backend.WithLimiter).
	be = backend.Throttle(be)
	r, err := repo.Open(ctx, be, repo.Password(rs.Password))
	if errors.Is(err, repo.ErrNotInitialized) && create {
		r, err = repo.Init(ctx, be, repo.Password(rs.Password))
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
	if run.NAS != nil {
		return a.withNAS(ctx, run, false, func(paths []string) api.RunResult {
			return a.backupPaths(ctx, run, paths, false)
		})
	}
	return a.backupPaths(ctx, run, run.Paths, a.VSS)
}

// backupPaths backs up local paths (or those of a mounted NAS share).
func (a *Agent) backupPaths(ctx context.Context, run api.Run, paths []string, useVSS bool) api.RunResult {
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
		Paths:    paths,
		Excludes: run.Excludes,
		Version:  a.version,
		Tags:     []string{fmt.Sprintf("run:%d", run.ID), jobTag(run.JobID)},
		VSS:      useVSS,
		Progress: func(_ string, s *repo.SnapshotStats) { a.progress(s.BytesRead, 0, s.Files) },
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
	if run.NAS != nil && run.RestoreTarget == "" {
		// Back to the share: mount it where the backup read it, writable.
		share := api.Run{NAS: run.NAS}
		run.NAS = nil
		return a.withNAS(ctx, share, true, func([]string) api.RunResult { return a.restore(ctx, run) })
	}
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
	return tlsutil.FetchFingerprint(ctx, u)
}

// SecureConfigDir restricts access to the directory of the default
// configuration to administrators. The service calls it on start, which
// also fixes installations made by older versions.
func SecureConfigDir(path string) error {
	if path != DefaultConfigPath() {
		return nil
	}
	return protectDir(filepath.Dir(path))
}

// pendingEnrollment is kept when an installer could not reach the console;
// the service retries until enrollment succeeds.
type pendingEnrollment struct {
	Server, Token, Fingerprint string
}

func pendingPath(cfgPath string) string {
	return filepath.Join(filepath.Dir(cfgPath), "pending-enrollment.json")
}

// SavePendingEnrollment stores enrollment data for later retries.
func SavePendingEnrollment(cfgPath, server, token, fingerprint string) error {
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		return err
	}
	if err := SecureConfigDir(cfgPath); err != nil {
		return err
	}
	b, _ := json.Marshal(pendingEnrollment{server, token, fingerprint})
	return os.WriteFile(pendingPath(cfgPath), b, 0o600)
}

// TryPendingEnrollment enrolls with stored data if there is any. It returns
// true when the agent is now enrolled.
func TryPendingEnrollment(ctx context.Context, cfgPath, version string) (bool, error) {
	b, err := os.ReadFile(pendingPath(cfgPath))
	if err != nil {
		return false, nil
	}
	var p pendingEnrollment
	if err := json.Unmarshal(b, &p); err != nil {
		os.Remove(pendingPath(cfgPath))
		return false, err
	}
	cfg, err := Enroll(ctx, p.Server, p.Token, p.Fingerprint, version)
	if err != nil {
		return false, err
	}
	if err := cfg.Save(cfgPath); err != nil {
		return false, err
	}
	os.Remove(pendingPath(cfgPath))
	return true, nil
}

// localIPs lists this machine's addresses for the console (no loopback or
// link-local addresses).
func localIPs() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() || ipn.IP.IsMulticast() {
			continue
		}
		out = append(out, ipn.IP.String())
	}
	return out
}
