package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/max-zit/backupzit/internal/repo"
)

// Four-eyes approval of job edits that weaken protection (B1, 0.62). Besides
// a shorter retention, an edit that makes a job run less (an automatic
// schedule to a chain or by hand), back up less (a source, VM, database or
// user removed; "all" replaced by a selection) or exclude more waits for a
// second user. Only the weakened settings wait; the rest of the edit is
// saved at once. The request names what changes, holds no secrets (no job
// options), and is refused when the job was changed again meanwhile, so an
// approval never overwrites a newer edit.

// protectSet are the settings of a job whose weakening needs approval.
type protectSet struct {
	Schedule  string               `json:"schedule"`
	Paths     []string             `json:"paths"`
	Excludes  []string             `json:"excludes"`
	Retention repo.RetentionPolicy `json:"retention"`
}

func protectOf(j Job) protectSet {
	return protectSet{Schedule: j.Schedule, Paths: nonNilStrings(j.Paths), Excludes: nonNilStrings(j.Excludes), Retention: j.Retention}
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// weakenReason is one way an edit weakens a job, for the approvals page:
// Format is English and translated there, Value is shown as it is.
type weakenReason struct {
	Format string `json:"format"`
	Value  string `json:"value,omitempty"`
}

// jobChange is the payload of a job.update request. Held names the
// settings that wait ("schedule", "paths", "excludes", "retention");
// Before has their values when it was requested, After the requested ones.
type jobChange struct {
	Held    []string       `json:"held"`
	Before  protectSet     `json:"before"`
	After   protectSet     `json:"after"`
	Reasons []weakenReason `json:"reasons"`
}

// JobChange is the change of a job.update request (approvals page).
func (a Approval) JobChange() jobChange {
	var c jobChange
	_ = json.Unmarshal(a.Payload, &c)
	return c
}

// ShorterRetention tells whether the change shortens the retention.
func (c jobChange) ShorterRetention() bool { return slices.Contains(c.Held, "retention") }

// scheduleRank: 2 runs by itself, 1 only after another job, 0 by hand.
func scheduleRank(s string) int {
	sc, err := ParseSchedule(s)
	if err != nil {
		return 2 // unknown (an old cron line): runs by itself
	}
	switch sc.Kind {
	case SchedManual:
		return 0
	case SchedChain:
		return 1
	}
	return 2
}

// allSources tells whether paths mean "everything" for a job of kind: all
// VMs, databases, Microsoft 365 users, or the whole share.
func allSources(kind string, paths []string) bool {
	switch kind {
	case JobVM:
		return slices.Contains(paths, "*")
	case JobSQL, JobM365, JobNAS:
		return len(paths) == 0
	}
	return false
}

// missing returns the items of a that are not in b.
func missing(a, b []string) []string {
	var out []string
	for _, x := range a {
		if !slices.Contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}

func listValue(items []string) string {
	v := strings.Join(items, ", ")
	if r := []rune(v); len(r) > 300 {
		v = string(r[:300]) + "…"
	}
	return v
}

// weakens compares the protection settings of a job (or a job for several
// agents) of kind before and after an edit. It returns the settings that
// weaken and why.
func weakens(kind string, old, cur protectSet) (held []string, reasons []weakenReason) {
	if scheduleRank(cur.Schedule) < scheduleRank(old.Schedule) {
		held = append(held, "schedule")
		if scheduleRank(cur.Schedule) == 0 {
			reasons = append(reasons, weakenReason{Format: "Runs only when started by hand from now on"})
		} else {
			reasons = append(reasons, weakenReason{Format: "Runs only after another job from now on"})
		}
	}
	switch {
	case allSources(kind, cur.Paths):
	case allSources(kind, old.Paths):
		held = append(held, "paths")
		reasons = append(reasons, weakenReason{Format: "Instead of everything, only: %s", Value: listValue(cur.Paths)})
	default:
		if gone := missing(old.Paths, cur.Paths); len(gone) > 0 {
			held = append(held, "paths")
			reasons = append(reasons, weakenReason{Format: "No longer backed up: %s", Value: listValue(gone)})
		}
	}
	if added := missing(cur.Excludes, old.Excludes); len(added) > 0 {
		held = append(held, "excludes")
		reasons = append(reasons, weakenReason{Format: "Excluded from now on: %s", Value: listValue(added)})
	}
	if shorterRetention(old.Retention, cur.Retention) {
		held = append(held, "retention")
	}
	return held, reasons
}

// holdWeakened splits an edit of a job: cur keeps the old values of the
// weakened settings (and is saved now); the returned change sets them
// once approved. ok is false when nothing weakens.
func holdWeakened(old Job, cur *Job) (jobChange, bool) {
	before, after := protectOf(old), protectOf(*cur)
	held, reasons := weakens(old.Kind, before, after)
	if len(held) == 0 {
		return jobChange{}, false
	}
	for _, f := range held {
		switch f {
		case "schedule":
			cur.Schedule = old.Schedule
		case "paths":
			cur.Paths = old.Paths
		case "excludes":
			cur.Excludes = old.Excludes
		case "retention":
			cur.Retention = old.Retention
		}
	}
	return jobChange{Held: held, Before: before, After: after, Reasons: reasons}, true
}

// errJobChangedMeanwhile: an approved change would overwrite a newer edit.
var errJobChangedMeanwhile = errors.New("the job was changed after the request; request the change again")

// applyJobChange carries out an approved job.update.
func (s *Store) applyJobChange(ctx context.Context, id int64, c jobChange) error {
	j, err := s.GetJob(ctx, id)
	if err != nil {
		return err
	}
	cur := protectOf(j)
	for _, f := range c.Held {
		var same bool
		switch f {
		case "schedule":
			same = cur.Schedule == c.Before.Schedule
			j.Schedule = c.After.Schedule
		case "paths":
			same = slices.Equal(cur.Paths, c.Before.Paths)
			j.Paths = c.After.Paths
		case "excludes":
			same = slices.Equal(cur.Excludes, c.Before.Excludes)
			j.Excludes = c.After.Excludes
		case "retention":
			same = cur.Retention == c.Before.Retention
			j.Retention = c.After.Retention
		default:
			return errors.New("unknown setting " + f)
		}
		if !same {
			return errJobChangedMeanwhile
		}
	}
	return s.UpdateJob(ctx, j)
}

// groupProtect are the protection settings of a job for several agents.
func groupProtect(g JobGroup) protectSet {
	return protectSet{Schedule: g.Schedule, Paths: nonNilStrings(g.Paths), Excludes: nonNilStrings(g.Excludes), Retention: g.Retention}
}

// groupUpdate is the payload of a group.update request (0.62; before, the
// payload was the JobGroup itself). Before is the fingerprint of the job
// when it was requested.
type groupUpdate struct {
	Group   JobGroup       `json:"group"`
	Before  string         `json:"before"`
	Reasons []weakenReason `json:"reasons,omitempty"`
}

// GroupUpdate is the change of a group.update request (approvals page).
func (a Approval) GroupUpdate() groupUpdate {
	var c groupUpdate
	_ = json.Unmarshal(a.Payload, &c)
	return c
}

// groupFingerprint identifies the settings of a job for several agents.
func groupFingerprint(g JobGroup) string {
	g.TargetName, g.CreatedAt = "", g.CreatedAt.UTC()
	b, _ := json.Marshal(g)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
