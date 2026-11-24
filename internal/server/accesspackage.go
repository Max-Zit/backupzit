package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/max-zit/backupzit/internal/recovery"
	"golang.org/x/crypto/bcrypt"
)

// Access packages hold repository locations and credentials, encrypted
// with a passphrase, for the recovery environment's offline mode (see
// internal/recovery). The console creates them and remembers when, with a
// fingerprint of each storage target's credentials, to warn when a
// package no longer matches.

const accessPackagesSetting = "access_packages"

// accessRecord is one access package the console created. It holds no
// secrets, only fingerprints; the list is still stored sealed (see
// saveAccessRecords).
type accessRecord struct {
	Created time.Time        `json:"created"`
	User    string           `json:"user"`
	Repos   int              `json:"repos"`
	Targets map[int64]string `json:"targets"` // target ID -> credentials fingerprint
}

// recoveryRepo is a repository with disk images that can go into an
// access package.
type recoveryRepo struct {
	TargetID   int64
	TargetName string
	URL        string
	Hostname   string
	Last       time.Time
}

// Key identifies the repository in the form.
func (r recoveryRepo) Key() string { return strconv.FormatInt(r.TargetID, 10) + "|" + r.URL }

// recoveryRepos lists the repositories that hold disk images or virtual
// machines, newest backup first per computer.
func (s *Store) recoveryRepos(ctx context.Context) ([]recoveryRepo, error) {
	rows, err := s.db.Query(ctx, `SELECT r.target_id, t.name, r.repo_url, COALESCE(a.hostname, ''), max(r.finished_at)
		FROM runs r JOIN storage_targets t ON t.id=r.target_id LEFT JOIN agents a ON a.id=r.agent_id
		WHERE r.repo_url<>'' AND r.kind IN ('image-backup','vm-backup','system-backup')
		  AND r.status IN ('success','warning') AND r.snapshot_id<>'' AND NOT r.expired
		GROUP BY r.target_id, t.name, r.repo_url, a.hostname
		ORDER BY COALESCE(a.hostname, ''), t.name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (recoveryRepo, error) {
		var x recoveryRepo
		var last *time.Time
		err := r.Scan(&x.TargetID, &x.TargetName, &x.URL, &x.Hostname, &last)
		if last != nil {
			x.Last = *last
		}
		return x, err
	})
}

// credentialsFingerprint changes whenever anything needed to open the
// target's repositories changes. It is an unsalted hash over the secrets,
// which is why the records holding it are only ever stored sealed: in the
// clear, a database dump would be enough to guess a human-chosen SFTP or
// SMB password offline against it (S1 of the review of PR #61). Sealed,
// the fingerprints are as safe as the credentials themselves, and records
// written by earlier builds keep comparing equal.
func credentialsFingerprint(t Target) string {
	h := sha256.New()
	for _, v := range []string{t.URL, t.SFTPPassword, t.SFTPKey, t.SFTPHostKey, t.S3AccessKey, t.S3SecretKey, t.S3Region,
		t.SMBPassword, t.SMBDomain, t.HardenedKey, t.HardenedFingerprint, t.AzureKey, t.AzureSAS, t.RecoveryKey} {
		h.Write([]byte(v))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// accessRecordsContext binds the sealed list to its setting, as
// sealSetting does for the settings in secretSettings.
const accessRecordsContext = "settings." + accessPackagesSetting

// saveAccessRecords stores the list of created packages sealed with the
// secrets box, in the same form sealSetting gives the settings in
// secretSettings (a JSON string "enc:v1:..."), so GetSetting and
// openSetting read it like any of those. Should the setting be added to
// secretSettings one day, SetSetting seals it itself and this does not
// seal twice.
func (s *Store) saveAccessRecords(ctx context.Context, records []accessRecord) error {
	if records == nil {
		records = []accessRecord{}
	}
	if s.box == nil || secretSettings[accessPackagesSetting] {
		return s.SetSetting(ctx, accessPackagesSetting, records)
	}
	b, err := json.Marshal(records)
	if err != nil {
		return err
	}
	return s.SetSetting(ctx, accessPackagesSetting, s.box.seal(accessRecordsContext, string(b)))
}

// loadAccessRecords reads the list of created packages. A list that an
// earlier build stored in the clear is sealed on the way: the update is
// conditional on the value read, so a package created meanwhile is not
// lost, and a failed update is tried again on the next read.
func (s *Store) loadAccessRecords(ctx context.Context) ([]accessRecord, error) {
	var raw []byte
	err := s.db.QueryRow(ctx, `SELECT value FROM settings WHERE key=$1`, accessPackagesSetting).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	plain, err := s.openSetting(accessPackagesSetting, raw)
	if err != nil {
		return nil, err
	}
	var records []accessRecord
	if err := json.Unmarshal(plain, &records); err != nil {
		return nil, err
	}
	if s.box != nil && !strings.HasPrefix(string(raw), `"`+secretPrefix) {
		sealed, err := json.Marshal(s.box.seal(accessRecordsContext, string(plain)))
		if err != nil {
			return nil, err
		}
		if _, err := s.db.Exec(ctx, `UPDATE settings SET value=$2, updated_at=now() WHERE key=$1 AND value=$3`,
			accessPackagesSetting, sealed, raw); err != nil {
			return nil, err
		}
	}
	return records, nil
}

// accessRepo turns a repository on a target into an access package entry.
func accessRepo(t Target, name, url string) recovery.AccessRepo {
	r := targetRepository(t, url)
	return recovery.AccessRepo{
		Name: name, Location: r.URL, Password: r.Password,
		SFTPPassword: r.SFTPPassword, SFTPKey: r.SFTPKey, SFTPHostKey: r.SFTPHostKey,
		S3AccessKey: r.S3AccessKey, S3SecretKey: r.S3SecretKey, S3Region: r.S3Region,
		SMBPassword: r.SMBPassword, SMBDomain: r.SMBDomain, AzureKey: r.AzureKey, AzureSAS: r.AzureSAS,
		HardenedKey: r.HardenedKey, HardenedFingerprint: r.HardenedFingerprint,
	}
}

// staleAccessPackages describes access packages whose credentials have
// changed since they were made (or whose storage was removed).
func staleAccessPackages(records []accessRecord, targets []Target) []string {
	byID := map[int64]Target{}
	for _, t := range targets {
		byID[t.ID] = t
	}
	var out []string
	for _, rec := range records {
		var changed []string
		for id, fp := range rec.Targets {
			t, ok := byID[id]
			switch {
			case !ok:
				changed = append(changed, fmt.Sprintf("#%d (removed)", id))
			case credentialsFingerprint(t) != fp:
				changed = append(changed, t.Name)
			}
		}
		if len(changed) > 0 {
			sort.Strings(changed)
			out = append(out, fmt.Sprintf("%s (%s): %s", rec.Created.Local().Format("2006-01-02 15:04"), rec.User, strings.Join(changed, ", ")))
		}
	}
	return out
}

// accessMessages are the console's texts for the reasons internal/recovery
// refuses a passphrase or a package. They are fixed sentences, so the
// catalogs translate them (render passes Error through T).
var accessMessages = []struct {
	err error
	msg string
}{
	{recovery.ErrPassphraseShort, "The passphrase is too short."},
	{recovery.ErrPassphraseControl, "The passphrase contains control characters."},
	{recovery.ErrPassphraseSpace, "The passphrase must not begin or end with a space."},
	{recovery.ErrPassphraseCommon, "The passphrase is a common password or made from one. Choose another one."},
	{recovery.ErrPassphraseKinds, "The passphrase needs more kinds of characters or more length."},
	{recovery.ErrPassphrasePredictable, "The passphrase is too predictable: it repeats characters or follows a sequence such as 1234 or qwerty."},
	{recovery.ErrAccessPackageTooLarge, "The access package would be larger than 1 MiB. Choose fewer backups."},
}

// msgConsolePassword refuses the signed-in user's own console password as
// the passphrase.
const msgConsolePassword = "The passphrase must not be your console password."

// accessMessage is the console text for err from internal/recovery.
func accessMessage(err error) string {
	for _, m := range accessMessages {
		if errors.Is(err, m.err) {
			return m.msg
		}
	}
	return err.Error()
}

// isConsolePassword reports whether pass is the signed-in user's console
// password: the one just typed to confirm (this covers directory users),
// or the local account's password when two-factor sign-in confirmed it.
// A package sealed with it would fall to whoever learns that password, and
// it would not change when the password does.
func (s *Server) isConsolePassword(r *http.Request, pass string) bool {
	if rp := r.FormValue("reauth_password"); rp != "" && subtleEqual([]byte(rp), []byte(pass)) {
		return true
	}
	me := currentUser(r)
	if me == nil || me.Source != "local" {
		return false
	}
	_, hash, err := s.store.userByName(r.Context(), me.Username)
	return err == nil && hash != "" && bcrypt.CompareHashAndPassword([]byte(hash), []byte(pass)) == nil
}

// handleAccessPackage creates an access package for the selected
// repositories and sends it as a download.
func (s *Server) handleAccessPackage(w http.ResponseWriter, r *http.Request, user string) {
	ctx := r.Context()
	fail := func(msg string) {
		s.recoveryPageWith(w, r, user, "", time.Time{}, msg)
	}
	if err := s.confirmIdentity(r); err != nil {
		fail(err.Error())
		return
	}
	pass := r.FormValue("passphrase")
	if pass != r.FormValue("passphrase2") {
		fail("The passphrases do not match.")
		return
	}
	if err := recovery.CheckPassphrase(pass); err != nil {
		fail(accessMessage(err))
		return
	}
	if s.isConsolePassword(r, pass) {
		fail(msgConsolePassword)
		return
	}
	repos, err := s.store.recoveryRepos(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	targets, err := s.store.ListTargets(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	byID := map[int64]Target{}
	for _, t := range targets {
		byID[t.ID] = t
	}
	want := map[string]bool{}
	for _, k := range r.Form["repo"] {
		want[k] = true
	}
	pkg := &recovery.AccessPackage{CreatedBy: user}
	rec := accessRecord{User: user, Targets: map[int64]string{}}
	for _, x := range repos {
		t, ok := byID[x.TargetID]
		if !want[x.Key()] || !ok {
			continue
		}
		name := x.Hostname
		if name == "" {
			name = x.URL
		}
		pkg.Repositories = append(pkg.Repositories, accessRepo(t, name+" – "+t.Name, x.URL))
		rec.Targets[t.ID] = credentialsFingerprint(t)
	}
	if len(pkg.Repositories) == 0 {
		fail("Choose at least one backup.")
		return
	}
	b, err := recovery.SealAccessPackage(pkg, pass)
	if err != nil {
		fail(accessMessage(err))
		return
	}
	rec.Created, rec.Repos = pkg.Created, len(pkg.Repositories)
	records, err := s.store.loadAccessRecords(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	records = append(records, rec)
	if len(records) > 50 {
		records = records[len(records)-50:]
	}
	if err := s.store.saveAccessRecords(ctx, records); err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, "recovery.access-package", "created an access package with %d repositories", len(pkg.Repositories))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="backupzit-`+pkg.Created.Format("20060102")+recovery.AccessPackageExt+`"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

// handleAccessPackagesForget clears the list of created access packages
// (after the old ones were destroyed), which ends the warnings.
func (s *Server) handleAccessPackagesForget(w http.ResponseWriter, r *http.Request, _ string) {
	if err := s.store.saveAccessRecords(r.Context(), nil); err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, "recovery.access-package", "cleared the list of access packages")
	http.Redirect(w, r, "/recovery", http.StatusSeeOther)
}
