package server

import (
	"context"
	"net/http"
	"time"

	"github.com/max-zit/backupzit/internal/update"
)

// The update banner: users who may install updates (Settings permission)
// see a newer console version at the top of every page. The version comes
// from the daily, signature-checked release check; nothing else is fetched.
// The × hides it for that user for five days, stored in the database so it
// holds in every browser; a version newer than the hidden one shows it again
// at once.

const updateBannerPause = 5 * 24 * time.Hour

// updateBanner is what the layout shows.
type updateBanner struct {
	Version   string // the newer release
	Installed string
	Container bool // updated by pulling a new image, not from the console
	NotesURL  string
}

// releaseNotesURL is the public page of a release. Every release the console
// accepts carries the BackupZit signature, so it is published there.
func releaseNotesURL(version string) string {
	return "https://github.com/Max-Zit/backupzit/releases/tag/v" + version
}

// showUpdateBanner decides whether a release is shown to a user who hid
// the banner for hiddenVersion until hiddenUntil.
func showUpdateBanner(available, hiddenVersion string, hiddenUntil *time.Time, now time.Time) bool {
	switch {
	case available == "":
		return false
	case hiddenVersion == "" || hiddenUntil == nil:
		return true
	case update.Newer(available, hiddenVersion):
		return true
	default:
		return !now.Before(*hiddenUntil)
	}
}

// availableRelease is the newer release the last check found, if any.
func (s *Server) availableRelease() string {
	if s.upd == nil {
		return ""
	}
	s.upd.mu.Lock()
	m := s.upd.available
	s.upd.mu.Unlock()
	if m == nil || !update.Newer(m.Version, s.Version) {
		return ""
	}
	return m.Version
}

// updateBannerFor returns the banner for u on the page at path, or nil.
func (s *Server) updateBannerFor(ctx context.Context, u *User, path string) *updateBanner {
	if u == nil || !u.Can(string(PermSettings)) || path == "/settings/updates" {
		return nil
	}
	v := s.availableRelease()
	if v == "" {
		return nil
	}
	hv, until, err := s.store.updateBannerHidden(ctx, u.ID)
	if err != nil {
		s.log.Warn("update banner", "err", err)
		hv, until = "", nil
	}
	if !showUpdateBanner(v, hv, until, s.clock()) {
		return nil
	}
	return &updateBanner{Version: v, Installed: s.Version, Container: InContainer(), NotesURL: releaseNotesURL(v)}
}

func (s *Store) updateBannerHidden(ctx context.Context, userID int64) (string, *time.Time, error) {
	var v string
	var until *time.Time
	err := s.db.QueryRow(ctx, `SELECT update_hidden_version, update_hidden_until FROM users WHERE id=$1`, userID).Scan(&v, &until)
	return v, until, err
}

// HideUpdateBanner hides the banner of version for a user until the given time.
func (s *Store) HideUpdateBanner(ctx context.Context, userID int64, version string, until time.Time) error {
	_, err := s.db.Exec(ctx, `UPDATE users SET update_hidden_version=$2, update_hidden_until=$3 WHERE id=$1`, userID, version, until)
	return err
}

// handleUpdateBannerHide serves POST /settings/updates/hide-banner and
// returns to the page the banner was on.
func (s *Server) handleUpdateBannerHide(w http.ResponseWriter, r *http.Request, _ string) {
	if u, v := currentUser(r), s.availableRelease(); u != nil && v != "" {
		if err := s.store.HideUpdateBanner(r.Context(), u.ID, v, s.clock().Add(updateBannerPause)); err != nil {
			s.serverError(w, err)
			return
		}
	}
	http.Redirect(w, r, refererPath(r), http.StatusSeeOther)
}
