package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The console in a container (the Docker image sets BACKUPZIT_CONTAINER):
// it is updated by pulling a new image, not from the web console, and the
// appliance's system settings do not apply. Before a new version changes
// the database, a copy of the console is kept in the data volume, so the
// previous image can be started again with that copy.

// InContainer reports whether the console runs from the container image.
func InContainer() bool { return os.Getenv("BACKUPZIT_CONTAINER") != "" }

// PreUpgradeDir holds the copies made before a new version started.
const PreUpgradeDir = "pre-upgrade"

// keepPreUpgrade is how many copies are kept.
const keepPreUpgrade = 3

// versionFile records the version that last started on the data directory.
const versionFile = "version"

// PreUpgradeCopy exports the database and the console's key files before a
// version other than the last one starts on this data directory. It returns
// the directory of the copy and the version it was made from, or "" when
// none was needed (same version, or a new installation).
func PreUpgradeCopy(ctx context.Context, dbURL, dataDir, version string) (dir, from string, err error) {
	vf := filepath.Join(dataDir, versionFile)
	prev, rerr := os.ReadFile(vf)
	from = strings.TrimSpace(string(prev))
	if rerr == nil && from == version {
		return "", "", nil
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return "", "", fmt.Errorf("connect database: %w", err)
	}
	defer pool.Close()
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		return "", "", fmt.Errorf("connect database: %w", err)
	}
	if !exists { // a new installation
		return "", "", os.WriteFile(vf, []byte(version+"\n"), 0o600)
	}
	if from == "" {
		from = "unknown"
	}
	dir = filepath.Join(dataDir, PreUpgradeDir, time.Now().UTC().Format("20060102-150405")+"-from-"+safeName(from))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	if err := exportConsole(ctx, pool, dataDir, from, dir); err != nil {
		os.RemoveAll(dir)
		return "", "", fmt.Errorf("copy of the console before the update: %w", err)
	}
	pruneDirs(filepath.Join(dataDir, PreUpgradeDir), keepPreUpgrade)
	return dir, from, os.WriteFile(vf, []byte(version+"\n"), 0o600)
}

// containerUpdated records that a new image started on an existing console
// (like the result of an update from the web console) and updates the
// agents when that is turned on.
func (s *Server) containerUpdated(ctx context.Context, from string) {
	if _, err := s.store.AppendAudit(ctx, "", "update.result", fmt.Sprintf("%s → %s: installed (container image)", from, s.Version), ""); err != nil {
		s.log.Error("audit log", "err", err)
	}
	if s.updateSettings(ctx).AutoAgents {
		s.updateAgentsAfterConsole(ctx, s.Version)
	}
}

// RestorePreUpgrade loads a copy made by PreUpgradeCopy (run it with the
// image of the version the copy was made from).
func RestorePreUpgrade(ctx context.Context, dbURL, dataDir, dir string, logf func(string, ...any)) error {
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	var meta consoleMeta
	if err != nil || json.Unmarshal(b, &meta) != nil {
		return errors.New("not a copy of the console (meta.json missing or damaged)")
	}
	if err := restoreConsoleRoot(ctx, dbURL, dataDir, dir, logf); err != nil {
		return err
	}
	// The data directory now belongs to the version of the copy: starting
	// that version makes no new copy.
	return os.WriteFile(filepath.Join(dataDir, versionFile), []byte(meta.Version+"\n"), 0o600)
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '.' || r == '-' {
			return r
		}
		return '_'
	}, s)
}

// pruneDirs keeps the newest n subdirectories (their names sort by time).
func pruneDirs(dir string, n int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for i := 0; i < len(names)-n; i++ {
		os.RemoveAll(filepath.Join(dir, names[i]))
	}
}

// SyncDist copies the agent installers that come with the image into the
// data volume, where the console offers them for new agents and updates.
func SyncDist(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		target := filepath.Join(dst, e.Name())
		if fi, err := os.Stat(target); err == nil && fi.Size() > 0 {
			if info, err := e.Info(); err == nil && info.Size() == fi.Size() {
				continue // already there (installers carry their version in the name)
			}
		}
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			return err
		}
		tmp := target + ".tmp"
		if err := os.WriteFile(tmp, b, 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp, target); err != nil {
			return err
		}
	}
	return nil
}
