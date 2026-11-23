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

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/update"
)

// updateDir holds downloaded installers.
func updateDir() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(filepath.Dir(DefaultConfigPath()), "updates")
	}
	// Root's own folder: in /var/tmp (before 0.54.1) any local user could
	// create the folder first and swap the checked package before dpkg -i.
	return "/var/lib/backupzit-agent/updates"
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
	// What to install is decided by the release signature, not by the
	// console: the installer must be in the signed list of its version, and
	// that list is checked with the release keys built into this agent
	// (GHSA-v284-8576-jp84). The console's checksum only has to agree.
	want, err := signedInstaller(run, a.version)
	if err != nil {
		return failed(err), nil
	}
	dir := updateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return failed(err), nil
	}
	if err := privateDir(dir); err != nil {
		return failed(err), nil
	}
	// Keep only the newest download.
	if old, _ := filepath.Glob(filepath.Join(dir, "backupzit-agent*")); old != nil {
		for _, f := range old {
			_ = os.Remove(f)
		}
	}
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) // #nosec G304 -- the agent's own configuration, update or work file
	if err != nil {
		return failed(err), nil
	}
	h := sha256.New()
	err = a.client.download(ctx, name, io.MultiWriter(f, h))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return failed(fmt.Errorf("download %s: %w", name, err)), nil
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, want.SHA256) || !strings.EqualFold(got, run.UpdateSHA256) {
		_ = os.Remove(path)
		return failed(fmt.Errorf("installer %s is damaged or was changed (SHA-256 %s, the signed release lists %s)", name, got, want.SHA256)), nil
	}
	install, err := installerCommand(path)
	if err != nil {
		_ = os.Remove(path)
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

// RetainCachePath is the service's file of known object lock dates: in its
// own folder, which only the service account can change (a forged date
// would skip extending a lock).
func RetainCachePath() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(filepath.Dir(DefaultConfigPath()), "retain-cache.json")
	}
	return "/var/lib/backupzit-agent/retain-cache.json"
}

// updateKeys are the release keys an update must be signed with (nil:
// update.TrustedKeys; tests replace them).
var updateKeys []string

// signedInstaller returns the signed release entry of the installer an
// update run names: the run must carry the signed list of the agent
// installers of its version, the list must verify with the release keys,
// name the installer, and the version must be newer than the agent's (a
// console must not downgrade agents to an older, signed but vulnerable
// release).
func signedInstaller(run api.Run, own string) (update.File, error) {
	if run.UpdateManifest == "" || run.UpdateManifestSig == "" {
		return update.File{}, fmt.Errorf("the console sent no signed list of the agent installers of %s; update the console to %s or newer (its package carries the list), then update the agent", run.UpdateVersion, "0.56.0")
	}
	if !update.Newer(run.UpdateVersion, strings.TrimSuffix(own, "-legacy")) {
		return update.File{}, fmt.Errorf("version %s is not newer than this agent's %s; agents are not downgraded", run.UpdateVersion, own)
	}
	f, err := update.VerifyAgentInstaller([]byte(run.UpdateManifest), run.UpdateManifestSig, updateKeys, run.UpdateVersion, run.UpdateFile)
	if err != nil {
		return update.File{}, fmt.Errorf("the update was refused: %w", err)
	}
	return f, nil
}
