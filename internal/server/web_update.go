package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/max-zit/backupzit/internal/update"
)

// Console self-update: the console checks a release source for signed
// releases, downloads the server package and asks the root helper
// (backupzit-update.path/.service) to install it.

const settingUpdates = "updates"

// DefaultUpdateSource is where official releases are published: the assets
// of the newest GitHub release (manifest.json, its signature, the packages).
const DefaultUpdateSource = "https://github.com/Max-Zit/backupzit/releases/latest/download"

// UpdateSettings are configured under Settings → Updates.
type UpdateSettings struct {
	// Source is an HTTPS address or a local folder with manifest.json,
	// manifest.json.sig and the packages.
	Source string `json:"source"`
	// AutoAgents updates outdated agents after the console was updated.
	AutoAgents bool `json:"auto_agents"`
	// Notify emails the administrators when a new version is available.
	Notify bool `json:"notify"`
}

type updater struct {
	dir    string // data directory/update
	helper bool   // the root helper is installed

	mu        sync.Mutex
	checked   time.Time
	checkErr  string
	available *update.Manifest
	raw       []byte
	sig       string
	notified  string // version already announced by email
}

// StartUpdates prepares self-update and checks for releases daily.
func (s *Server) StartUpdates(ctx context.Context, dataDir string) {
	s.upd = &updater{dir: filepath.Join(dataDir, "update"), helper: updateHelperInstalled()}
	os.MkdirAll(s.upd.dir, 0o750)
	s.reportUpdateResult(ctx)
	// The helper writes its result after the new version has started (or
	// after the rollback), so look for it again for a while.
	go func() {
		for i := 0; i < 20; i++ {
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
			}
			s.reportUpdateResult(ctx)
		}
	}()
	go func() {
		select { // let the console start first
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
		for {
			s.checkUpdates(ctx)
			if s.upd.helper { // refresh the operating system status daily
				s.requestOS(ctx, update.ActionOSStatus, false, "")
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(24 * time.Hour):
			}
		}
	}()
}

func updateHelperInstalled() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	for _, p := range []string{"/usr/lib/systemd/system/backupzit-update.path", "/lib/systemd/system/backupzit-update.path"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// packageFormat is the package type of this machine.
func packageFormat() string {
	if _, err := exec.LookPath("dpkg"); err == nil {
		return "deb"
	}
	return "rpm"
}

func (s *Server) updateSettings(ctx context.Context) UpdateSettings {
	u := UpdateSettings{Notify: true, AutoAgents: true}
	s.store.GetSetting(ctx, settingUpdates, &u)
	if u.Source == "" {
		u.Source = DefaultUpdateSource
	}
	return u
}

// checkUpdates reads the release source and remembers a newer release.
func (s *Server) checkUpdates(ctx context.Context) error {
	cfg := s.updateSettings(ctx)
	u := s.upd
	if cfg.Source == "" {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	m, raw, sig, err := update.Source{Base: cfg.Source}.Latest(cctx)
	if err != nil && strings.Contains(err.Error(), "404") {
		if strings.TrimRight(cfg.Source, "/") == DefaultUpdateSource {
			err = errors.New("no BackupZit release is available from GitHub yet (github.com/Max-Zit/backupzit has no public release); try again later or install an update from files")
		} else {
			err = fmt.Errorf("the update source has no manifest.json (%s): check the address", strings.TrimRight(cfg.Source, "/")+"/manifest.json")
		}
	}
	u.mu.Lock()
	u.checked = time.Now()
	if err != nil {
		u.checkErr = err.Error()
		u.mu.Unlock()
		return err
	}
	u.checkErr = ""
	if update.Newer(m.Version, s.Version) {
		u.available, u.raw, u.sig = m, raw, sig
	} else {
		u.available, u.raw, u.sig = nil, nil, ""
	}
	announce := u.available != nil && cfg.Notify && u.notified != m.Version
	if announce {
		u.notified = m.Version
	}
	u.mu.Unlock()
	if announce {
		s.mailAdmins(ctx, "[BackupZit] Version "+m.Version+" is available",
			fmt.Sprintf("BackupZit %s is available (installed: %s).\n\n%s\n\nInstall it under Settings → Updates.\n", m.Version, s.Version, m.Notes))
	}
	return nil
}

// reportUpdateResult records the outcome of the last update once, and
// updates the agents after a successful console update.
func (s *Server) reportUpdateResult(ctx context.Context) {
	var res update.Result
	if update.ReadJSON(filepath.Join(s.upd.dir, "result.json"), &res) != nil {
		return
	}
	var seen time.Time
	s.store.GetSetting(ctx, "update_result_seen", &seen)
	if !res.Finished.After(seen) {
		return
	}
	s.store.SetSetting(ctx, "update_result_seen", res.Finished)
	detail := fmt.Sprintf("%s → %s: %s", res.From, res.Version, res.Status)
	if _, err := s.store.AppendAudit(ctx, "", "update.result", detail+": "+res.Message, ""); err != nil {
		s.log.Error("audit log", "err", err)
	}
	if res.Status != "installed" {
		s.mailAdmins(ctx, "[BackupZit] Console update to "+res.Version+" "+res.Status, res.Message+"\n")
		return
	}
	if s.updateSettings(ctx).AutoAgents {
		agents, err := s.store.ListAgents(ctx)
		if err != nil {
			return
		}
		n := 0
		for id, u := range s.availableUpdates(agents) {
			if _, err := s.store.QueueAgentUpdate(ctx, id, u); err == nil {
				n++
			}
		}
		s.log.Info("console updated; agent updates queued", "version", res.Version, "agents", n)
	}
}

// updatePage is what the Updates tab shows.
type updatePage struct {
	Settings      UpdateSettings
	DefaultSource string
	Version       string
	Helper        bool
	Checked       time.Time
	CheckErr      string
	Available     *update.Manifest
	Pending       bool
	Last          *update.Result
}

func (s *Server) updatePageData(ctx context.Context) updatePage {
	p := updatePage{Version: s.Version, DefaultSource: DefaultUpdateSource, Settings: s.updateSettings(ctx)}
	if s.upd == nil {
		return p
	}
	u := s.upd
	u.mu.Lock()
	p.Helper, p.Checked, p.CheckErr, p.Available = u.helper, u.checked, u.checkErr, u.available
	u.mu.Unlock()
	if _, err := os.Stat(filepath.Join(u.dir, "request.json")); err == nil {
		p.Pending = true
	}
	var res update.Result
	if update.ReadJSON(filepath.Join(u.dir, "result.json"), &res) == nil {
		p.Last = &res
	}
	return p
}

func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request, _ string) {
	const back = "/settings/updates"
	src := strings.TrimSpace(r.FormValue("source"))
	if src != "" && !strings.HasPrefix(src, "https://") && !strings.HasPrefix(src, "file://") && !filepath.IsAbs(src) {
		redirectErr(w, r, back, errors.New("the update source is an https:// address or a folder on the server (/path or file:///path)"))
		return
	}
	cfg := UpdateSettings{Source: src, AutoAgents: r.FormValue("auto_agents") == "on", Notify: r.FormValue("notify") == "on"}
	if err := s.store.SetSetting(r.Context(), settingUpdates, cfg); err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, "settings.updates", "source %q, agents automatically %v", cfg.Source, cfg.AutoAgents)
	if cfg.Source != "" && s.upd != nil {
		if err := s.checkUpdates(r.Context()); err != nil {
			redirectErr(w, r, back, fmt.Errorf("saved, but the source cannot be read: %v", err))
			return
		}
	}
	redirectMsg(w, r, back, "Update settings saved.")
}

func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request, _ string) {
	const back = "/settings/updates"
	if s.upd == nil || s.updateSettings(r.Context()).Source == "" {
		redirectErr(w, r, back, errors.New("set an update source first"))
		return
	}
	if err := s.checkUpdates(r.Context()); err != nil {
		redirectErr(w, r, back, err)
		return
	}
	p := s.updatePageData(r.Context())
	if p.Available == nil {
		redirectMsg(w, r, back, "BackupZit "+s.Version+" is up to date.")
		return
	}
	redirectMsg(w, r, back, "Version "+p.Available.Version+" is available.")
}

// stageUpdate checks a release and hands its server package to the root helper.
func (s *Server) stageUpdate(ctx context.Context, m *update.Manifest, raw []byte, sig string, fetch func(f update.File, dst string) error, user string) error {
	u := s.upd
	if !u.helper {
		return errors.New("updates from the console need the Linux package of the console (the backupzit-update service is not installed); update with the package manager instead")
	}
	if !update.Newer(m.Version, s.Version) {
		return fmt.Errorf("version %s is not newer than the installed %s", m.Version, s.Version)
	}
	f, ok := m.Find("server", packageFormat(), "amd64")
	if !ok {
		return fmt.Errorf("the release has no %s package of the console", packageFormat())
	}
	var running int
	s.store.db.QueryRow(ctx, `SELECT count(*) FROM runs WHERE status='running'`).Scan(&running)
	if running > 0 {
		return fmt.Errorf("%d backups or restores are running; install the update when they have finished", running)
	}
	if _, err := os.Stat(filepath.Join(u.dir, "request.json")); err == nil {
		return errors.New("an update is already being installed")
	}
	if err := fetch(f, filepath.Join(u.dir, f.Name)); err != nil {
		return fmt.Errorf("download %s: %w", f.Name, err)
	}
	return update.WriteJSON(filepath.Join(u.dir, "request.json"),
		update.Request{Manifest: raw, Signature: sig, File: f.Name, Requested: time.Now().UTC(), User: user}, 0o640)
}

func (s *Server) handleUpdateInstall(w http.ResponseWriter, r *http.Request, user string) {
	const back = "/settings/updates"
	if s.upd == nil {
		redirectErr(w, r, back, errors.New("updates are not available in this mode"))
		return
	}
	s.upd.mu.Lock()
	m, raw, sig := s.upd.available, s.upd.raw, s.upd.sig
	s.upd.mu.Unlock()
	if m == nil {
		redirectErr(w, r, back, errors.New("no newer version is known; check for updates first"))
		return
	}
	src := update.Source{Base: s.updateSettings(r.Context()).Source}
	fetch := func(f update.File, dst string) error { return src.Download(r.Context(), f, dst) }
	if err := s.stageUpdate(r.Context(), m, raw, sig, fetch, user); err != nil {
		redirectErr(w, r, back, err)
		return
	}
	s.audit(r, "update.install", "%s → %s", s.Version, m.Version)
	redirectMsg(w, r, back, "Installing BackupZit "+m.Version+". The console restarts within a few minutes; reload this page then.")
}

// handleUpdateUpload installs a release uploaded as files (no internet access).
func (s *Server) handleUpdateUpload(w http.ResponseWriter, r *http.Request, user string) {
	const back = "/settings/updates"
	if s.upd == nil {
		redirectErr(w, r, back, errors.New("updates are not available in this mode"))
		return
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		redirectErr(w, r, back, errors.New("upload manifest.json, manifest.json.sig and the server package"))
		return
	}
	read := func(name string, max int64) ([]byte, error) {
		f, _, err := r.FormFile(name)
		if err != nil {
			return nil, fmt.Errorf("%s is missing", name)
		}
		defer f.Close()
		return io.ReadAll(io.LimitReader(f, max))
	}
	raw, err1 := read("manifest", 1<<20)
	sigB, err2 := read("signature", 4096)
	if err := errors.Join(err1, err2); err != nil {
		redirectErr(w, r, back, err)
		return
	}
	sig := strings.TrimSpace(string(sigB))
	m, err := update.Verify(raw, sig, nil)
	if err != nil {
		redirectErr(w, r, back, err)
		return
	}
	fetch := func(f update.File, dst string) error {
		in, _, err := r.FormFile("package")
		if err != nil {
			return errors.New("the server package is missing")
		}
		defer in.Close()
		out, err := os.OpenFile(dst+".part", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		out.Close()
		if err := update.CheckFile(dst+".part", f); err != nil {
			os.Remove(dst + ".part")
			return err
		}
		return os.Rename(dst+".part", dst)
	}
	if err := s.stageUpdate(r.Context(), m, raw, sig, fetch, user); err != nil {
		redirectErr(w, r, back, err)
		return
	}
	s.audit(r, "update.install", "%s → %s (uploaded)", s.Version, m.Version)
	redirectMsg(w, r, back, "Installing BackupZit "+m.Version+". The console restarts within a few minutes; reload this page then.")
}

// osPage is the operating system part of the Updates tab.
type osPage struct {
	Status  *update.OSStatus
	Result  *update.OSResult
	Pending bool
}

func (s *Server) osPageData() osPage {
	var p osPage
	if s.upd == nil {
		return p
	}
	var st update.OSStatus
	if update.ReadJSON(filepath.Join(s.upd.dir, "os-status.json"), &st) == nil {
		p.Status = &st
	}
	var res update.OSResult
	if update.ReadJSON(filepath.Join(s.upd.dir, "os-result.json"), &res) == nil {
		p.Result = &res
	}
	if _, err := os.Stat(filepath.Join(s.upd.dir, "request.json")); err == nil {
		p.Pending = true
	}
	return p
}

// requestOS hands an operating system task to the root helper.
func (s *Server) requestOS(ctx context.Context, action string, enable bool, user string) error {
	if s.upd == nil || !s.upd.helper {
		return errors.New("operating system updates need the Linux package of the console")
	}
	if _, err := os.Stat(filepath.Join(s.upd.dir, "request.json")); err == nil {
		return errors.New("another update task is in progress; try again in a minute")
	}
	if action == update.ActionReboot {
		var running int
		s.store.db.QueryRow(ctx, `SELECT count(*) FROM runs WHERE status='running'`).Scan(&running)
		if running > 0 {
			return fmt.Errorf("%d backups or restores are running; restart the server when they have finished", running)
		}
	}
	return update.WriteJSON(filepath.Join(s.upd.dir, "request.json"),
		update.Request{Action: action, Enable: enable, Requested: time.Now().UTC(), User: user}, 0o640)
}

func (s *Server) handleOSAction(w http.ResponseWriter, r *http.Request, user string) {
	const back = "/settings/updates#os"
	var action, msg string
	enable := false
	switch r.FormValue("action") {
	case "status":
		action, msg = update.ActionOSStatus, "Checking for operating system updates; reload the page in a minute."
	case "auto-on":
		action, enable, msg = update.ActionOSAuto, true, "Switching on automatic security updates."
	case "auto-off":
		action, msg = update.ActionOSAuto, "Switching off automatic security updates."
	case "upgrade":
		action, msg = update.ActionOSUpgrade, "Installing all operating system updates; this can take a few minutes."
	case "reboot":
		action, msg = update.ActionReboot, "The server restarts now; the console is back in a few minutes."
	default:
		redirectErr(w, r, back, errors.New("unknown action"))
		return
	}
	if err := s.requestOS(r.Context(), action, enable, user); err != nil {
		redirectErr(w, r, back, err)
		return
	}
	s.audit(r, "os."+r.FormValue("action"), "")
	redirectMsg(w, r, back, msg)
}
