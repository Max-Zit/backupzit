package server

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// settingsLock is the first key of the advisory locks that serialise
// changes of one setting (the second key is the setting's name).
const settingsLock int32 = 7316542

// updateSettingMap changes the JSON map stored under setting key in one
// transaction: other writers of the same setting wait (an advisory lock per
// key, which also covers a row that does not exist yet), fn changes the
// current entries, and nothing is written when the read fails or fn reports
// no change. copy_stalls and schedule_misses were read, changed and written
// whole by the agents' polls, the scheduler and the notifier at once, so
// one writer's entries were lost, and a failed read wrote a map with only
// the new entry over all others (LOW).
func updateSettingMap[T any](ctx context.Context, s *Store, key string, fn func(m map[string]T) bool) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, hashtext($2))`, settingsLock, key); err != nil {
		return err
	}
	m := map[string]T{}
	var b []byte
	err = tx.QueryRow(ctx, `SELECT value FROM settings WHERE key=$1`, key).Scan(&b)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return err
	default:
		if b, err = s.openSetting(key, b); err != nil {
			return err
		}
		if err := json.Unmarshal(b, &m); err != nil {
			return err
		}
		if m == nil { // stored as null
			m = map[string]T{}
		}
	}
	if !fn(m) {
		return nil
	}
	if b, err = json.Marshal(m); err != nil {
		return err
	}
	if b, err = s.sealSetting(key, b); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO settings(key, value, updated_at) VALUES($1,$2,now())
		ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, updated_at=now()`, key, b); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// settleNotices records a notifier pass over the map of setting key, read
// before as seen: the entries it dropped or notified are changed only while
// they are still the ones it read (the same since); what other writers
// added or changed meanwhile stays. The notifier sends while it holds no
// lock (sending takes seconds) and does not write its old copy back.
func settleNotices[T any](ctx context.Context, s *Store, key string, seen map[string]T, drop, notified []string,
	since func(T) time.Time, markNotified func(T) T) error {
	if len(drop) == 0 && len(notified) == 0 {
		return nil
	}
	return updateSettingMap(ctx, s, key, func(m map[string]T) bool {
		changed := false
		for _, k := range drop {
			if cur, ok := m[k]; ok && since(cur).Equal(since(seen[k])) {
				delete(m, k)
				changed = true
			}
		}
		for _, k := range notified {
			if cur, ok := m[k]; ok && since(cur).Equal(since(seen[k])) {
				m[k] = markNotified(cur)
				changed = true
			}
		}
		return changed
	})
}
