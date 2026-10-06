package update

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Applier installs a requested console update. It runs as root (started by
// the backupzit-update systemd path unit) and trusts nothing the console
// wrote: the manifest signature and the package checksum are verified
// again, on a copy in a directory only root can write.
type Applier struct {
	// Dir is where the console puts request.json and the package, and
	// where result.json is written.
	Dir string
	// Work is root's own directory for the verified copy, database dumps
	// and the previous binary.
	Work string
	// Binary is the installed server program (/usr/bin/backupzit-server).
	Binary string
	// CurrentVersion is the running version.
	CurrentVersion string
	// DBURL is the console's PostgreSQL database.
	DBURL string
	// HealthURL must answer 200 after the update (the sign-in page).
	HealthURL string
	// Service is the systemd unit of the console.
	Service string
	Keys    []string // nil: TrustedKeys
	// Run executes a command (tests replace it).
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
	// HealthWait is how long the new version may take to answer.
	HealthWait time.Duration
	Log        func(format string, args ...any)
	// OSFamily forces "apt" or "dnf"; Root prefixes system paths (tests).
	OSFamily string
	Root     string
}

func runCmd(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (a *Applier) logf(format string, args ...any) {
	if a.Log != nil {
		a.Log(format, args...)
	}
}

// Apply handles a pending request.json, if any, and writes result.json.
func (a *Applier) Apply(ctx context.Context) (*Result, error) {
	if a.Run == nil {
		a.Run = runCmd
	}
	if a.HealthWait == 0 {
		a.HealthWait = 2 * time.Minute
	}
	reqPath := filepath.Join(a.Dir, "request.json")
	var req Request
	if err := ReadJSON(reqPath, &req); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		os.Remove(reqPath)
		return a.finish(&Result{Status: "failed", From: a.CurrentVersion, Message: "invalid update request: " + err.Error()})
	}
	// The request is consumed in any case, so a bad one does not loop.
	os.Remove(reqPath)
	if req.Action == ActionNetwork || req.Action == ActionSSH || req.Action == ActionTimeZone {
		return a.netAction(ctx, req)
	}
	if req.Action != ActionInstall {
		return a.osAction(ctx, req)
	}
	res := &Result{From: a.CurrentVersion}
	m, err := Verify(req.Manifest, req.Signature, a.Keys)
	if err != nil {
		res.Status, res.Message = "failed", err.Error()
		return a.finish(res)
	}
	res.Version = m.Version
	var f *File
	for i := range m.Files {
		if m.Files[i].Name == req.File {
			f = &m.Files[i]
		}
	}
	if f == nil || f.Kind != "server" || (f.Format != "deb" && f.Format != "rpm") {
		res.Status, res.Message = "failed", "the request names no server package of the release"
		return a.finish(res)
	}
	if !Newer(m.Version, a.CurrentVersion) {
		res.Status, res.Message = "failed", fmt.Sprintf("version %s is not newer than the installed %s", m.Version, a.CurrentVersion)
		return a.finish(res)
	}
	if err := os.MkdirAll(a.Work, 0o700); err != nil {
		res.Status, res.Message = "failed", err.Error()
		return a.finish(res)
	}
	// Copy the package into root's directory, then verify the copy.
	pkg := filepath.Join(a.Work, f.Name)
	if err := copyRegular(filepath.Join(a.Dir, f.Name), pkg); err != nil {
		res.Status, res.Message = "failed", "read the package: "+err.Error()
		return a.finish(res)
	}
	if err := CheckFile(pkg, *f); err != nil {
		os.Remove(pkg)
		res.Status, res.Message = "failed", err.Error()
		return a.finish(res)
	}
	os.Remove(filepath.Join(a.Dir, f.Name))

	stamp := time.Now().UTC().Format("20060102-150405")
	dump := filepath.Join(a.Work, fmt.Sprintf("db-before-%s-%s.dump", m.Version, stamp))
	a.logf("backing up the database to %s", dump)
	if _, err := a.Run(ctx, "pg_dump", "--format=custom", "--file="+dump, "--dbname="+a.DBURL); err != nil {
		res.Status, res.Message = "failed", "database backup failed, nothing was changed: "+err.Error()
		return a.finish(res)
	}
	res.DBBackup = dump
	prev := filepath.Join(a.Work, "backupzit-server."+a.CurrentVersion)
	if err := copyRegular(a.Binary, prev); err != nil {
		res.Status, res.Message = "failed", "keep the current program: "+err.Error()
		return a.finish(res)
	}
	a.logf("installing %s", f.Name)
	install := []string{"dpkg", "-i", pkg}
	if f.Format == "rpm" {
		install = []string{"rpm", "-U", "--replacepkgs", pkg}
	}
	_, ierr := a.Run(ctx, install[0], install[1:]...)
	if ierr == nil {
		_, ierr = a.Run(ctx, "systemctl", "restart", a.Service)
	}
	if ierr == nil {
		ierr = a.healthy(ctx)
	}
	if ierr == nil {
		res.Status, res.Message = "installed", "BackupZit "+m.Version+" installed"
		a.prune()
		// The appliance's login screen reflects the restarted console (it
		// shows the initial password only while that is still valid).
		if _, err := os.Stat(filepath.Join(a.Root, "/usr/local/sbin/backupzit-appliance-boot")); err == nil {
			a.Run(ctx, "/usr/local/sbin/backupzit-appliance-boot", "issue")
		}
		return a.finish(res)
	}
	// Roll back: previous program and the database as it was.
	a.logf("update failed, rolling back: %v", ierr)
	logs, _ := a.Run(ctx, "journalctl", "-u", a.Service, "-n", "15", "--no-pager")
	_, err1 := a.Run(ctx, "systemctl", "stop", a.Service)
	err2 := copyRegular(prev, a.Binary)
	os.Chmod(a.Binary, 0o755)
	_, err3 := a.Run(ctx, "pg_restore", "--clean", "--if-exists", "--no-owner", "--dbname="+a.DBURL, dump)
	_, err4 := a.Run(ctx, "systemctl", "start", a.Service)
	res.Status = "rolled-back"
	res.Message = fmt.Sprintf("installing %s failed (%v); version %s and its database were restored", m.Version, ierr, a.CurrentVersion)
	if e := errors.Join(err1, err2, err3, err4); e != nil {
		res.Status = "failed"
		res.Message += "; the rollback had errors: " + e.Error()
	}
	if len(logs) > 0 {
		res.Message += "\n\nLog of the failed start:\n" + strings.TrimSpace(string(logs))
	}
	return a.finish(res)
}

func (a *Applier) finish(res *Result) (*Result, error) {
	res.Finished = time.Now().UTC()
	if res.Version == "" {
		res.Version = "?"
	}
	a.logf("update %s: %s", res.Status, res.Message)
	return res, WriteJSON(filepath.Join(a.Dir, "result.json"), res, 0o644)
}

// healthy waits until the console answers its sign-in page.
func (a *Applier) healthy(ctx context.Context) error {
	if a.HealthURL == "" {
		return nil
	}
	cl := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} // local check of our own service
	deadline := time.Now().Add(a.HealthWait)
	var last error
	for time.Now().Before(deadline) {
		resp, err := cl.Get(a.HealthURL)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			err = fmt.Errorf("status %s", resp.Status)
		}
		last = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("the new version did not start: %v", last)
}

// prune keeps the three newest database dumps and previous programs.
func (a *Applier) prune() {
	for _, pattern := range []string{"db-before-*.dump", "backupzit-server.*"} {
		files, _ := filepath.Glob(filepath.Join(a.Work, pattern))
		sort.Slice(files, func(i, j int) bool {
			fi, _ := os.Stat(files[i])
			fj, _ := os.Stat(files[j])
			return fi != nil && fj != nil && fi.ModTime().After(fj.ModTime())
		})
		for i, f := range files {
			if i >= 3 {
				os.Remove(f)
			}
		}
	}
	pkgs, _ := filepath.Glob(filepath.Join(a.Work, "*.deb"))
	rpms, _ := filepath.Glob(filepath.Join(a.Work, "*.rpm"))
	for _, p := range append(pkgs, rpms...) {
		os.Remove(p)
	}
}

// copyRegular copies a regular file (no symlinks or devices).
func copyRegular(src, dst string) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", src)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
