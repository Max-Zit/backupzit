package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/backupzit/backupzit/internal/api"
)

// updateDir holds downloaded installers.
func updateDir() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(filepath.Dir(DefaultConfigPath()), "updates")
	}
	return "/var/tmp/backupzit-updates"
}

// download fetches an agent installer from the console.
func (c *Client) download(ctx context.Context, name string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.ServerURL+api.PathDownloadPrefix+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.AgentUUID+":"+c.cfg.Secret)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server: HTTP %d", resp.StatusCode)
	}
	_, err = io.Copy(w, io.LimitReader(resp.Body, 1<<30))
	return err
}

// selfUpdate downloads and checks the installer; the installation starts
// after the result is reported, because it restarts the agent.
func (a *Agent) selfUpdate(ctx context.Context, run api.Run) (api.RunResult, func()) {
	name := run.UpdateFile
	if name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		return failed(fmt.Errorf("invalid installer name %q", name)), nil
	}
	if strings.TrimSuffix(a.version, "-legacy") == run.UpdateVersion {
		return api.RunResult{Status: api.StatusSuccess, Message: "Already running version " + a.version}, nil
	}
	dir := updateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return failed(err), nil
	}
	// Keep only the newest download.
	if old, _ := filepath.Glob(filepath.Join(dir, "backupzit-agent*")); old != nil {
		for _, f := range old {
			os.Remove(f)
		}
	}
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return failed(err), nil
	}
	h := sha256.New()
	err = a.client.download(ctx, name, io.MultiWriter(f, h))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		return failed(fmt.Errorf("download %s: %w", name, err)), nil
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, run.UpdateSHA256) {
		os.Remove(path)
		return failed(fmt.Errorf("installer %s is damaged or was changed (SHA-256 %s, expected %s)", name, got, run.UpdateSHA256)), nil
	}
	install, err := installerCommand(path)
	if err != nil {
		os.Remove(path)
		return failed(err), nil
	}
	res := api.RunResult{Status: api.StatusSuccess,
		Message: fmt.Sprintf("Downloaded and verified %s; installing version %s now — the agent restarts and reports the new version within a few minutes", name, run.UpdateVersion)}
	return res, func() {
		a.log.Info("starting agent update", "installer", path)
		if err := install(); err != nil {
			a.log.Error("start agent update", "err", err)
		}
	}
}
