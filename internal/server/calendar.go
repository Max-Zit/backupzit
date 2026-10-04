package server

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/max-zit/backupzit/internal/api"
)

// CalEntry is one job on one calendar day: planned runs (future) or
// finished runs (past).
type CalEntry struct {
	JobID    int64
	JobName  string
	Hostname string
	First    time.Time
	Last     time.Time
	Count    int
	Planned  bool
	Status   string // past: worst status of the day
	Kind     string
}

func (e CalEntry) TimeLabel() string {
	if e.Count > 1 {
		return fmt.Sprintf("%s–%s ×%d", e.First.Format("15:04"), e.Last.Format("15:04"), e.Count)
	}
	return e.First.Format("15:04")
}

type CalDay struct {
	Date    time.Time
	InMonth bool
	Today   bool
	Past    bool
	Entries []CalEntry
}

type calendarData struct {
	Month      time.Time
	Prev, Next string
	Weeks      [][]CalDay
	Upcoming   []CalEntry
	Agents     []Agent
	AgentID    int64
	Planned    int
}

var statusRank = map[string]int{api.StatusSuccess: 1, api.StatusWarning: 2, api.StatusFailed: 3, api.StatusRunning: 0, api.StatusQueued: 0}

func (s *Server) handleCalendar(w http.ResponseWriter, r *http.Request, user string) {
	now := time.Now()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.Local)
	if m, err := time.ParseInLocation("2006-01", r.URL.Query().Get("month"), time.Local); err == nil {
		month = m
	}
	agentID, _ := strconv.ParseInt(r.URL.Query().Get("agent"), 10, 64)
	ctx := r.Context()
	jobs, err := s.store.ListJobs(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	agents, err := s.store.ListAgents(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	// The grid covers whole weeks, Monday first.
	start := weekStart(month)
	end := weekStart(month.AddDate(0, 1, 0).AddDate(0, 0, -1)).AddDate(0, 0, 7)
	today := midnight(now)

	type key struct {
		day string
		job int64
	}
	entries := map[key]*CalEntry{}
	add := func(t time.Time, e CalEntry) {
		k := key{t.Format("2006-01-02"), e.JobID}
		x := entries[k]
		if x == nil {
			e.First, e.Last, e.Count = t, t, 1
			entries[k] = &e
			return
		}
		x.Count++
		if t.Before(x.First) {
			x.First = t
		}
		if t.After(x.Last) {
			x.Last = t
		}
		if statusRank[e.Status] > statusRank[x.Status] {
			x.Status = e.Status
		}
	}

	// Past: what actually ran.
	if start.Before(now) {
		runs, err := s.store.ListRunsBetween(ctx, start, minTime(end, now), agentID, 0)
		if err != nil {
			s.serverError(w, err)
			return
		}
		for _, run := range runs {
			if !(isBackupKind(run.Kind) || run.Kind == api.KindCopy) || run.JobID == nil {
				continue
			}
			add(run.QueuedAt.Local(), CalEntry{JobID: *run.JobID, JobName: deref(run.JobName), Hostname: run.Hostname, Status: run.Status, Kind: run.Kind})
		}
	}
	// Future: what the schedules will start.
	planned := 0
	var upcoming []CalEntry
	for _, j := range jobs {
		if !j.Enabled || (agentID != 0 && j.AgentID != agentID) {
			continue
		}
		t := maxTime(start, now)
		for n := 0; n < 5000; n++ {
			t = NextRun(j.Schedule, t)
			if t.IsZero() || !t.Before(maxTime(end, now.AddDate(0, 0, 7))) {
				break
			}
			e := CalEntry{JobID: j.ID, JobName: j.Name, Hostname: j.Hostname, Planned: true, Kind: j.Kind}
			if t.Before(now.AddDate(0, 0, 7)) && len(upcoming) < 500 {
				e.First, e.Last, e.Count = t, t, 1
				upcoming = append(upcoming, e)
			}
			if t.Before(end) {
				add(t.Local(), e)
				planned++
			}
		}
	}
	sort.Slice(upcoming, func(a, b int) bool { return upcoming[a].First.Before(upcoming[b].First) })
	if len(upcoming) > 40 {
		upcoming = upcoming[:40]
	}

	byDay := map[string][]CalEntry{}
	for k, e := range entries {
		byDay[k.day] = append(byDay[k.day], *e)
	}
	var weeks [][]CalDay
	for d := start; d.Before(end); d = d.AddDate(0, 0, 7) {
		var week []CalDay
		for i := 0; i < 7; i++ {
			day := d.AddDate(0, 0, i)
			list := byDay[day.Format("2006-01-02")]
			sort.Slice(list, func(a, b int) bool { return list[a].First.Before(list[b].First) })
			week = append(week, CalDay{Date: day, InMonth: day.Month() == month.Month(), Today: day.Equal(today),
				Past: day.Before(today), Entries: list})
		}
		weeks = append(weeks, week)
	}
	data := calendarData{Month: month, Weeks: weeks, Upcoming: upcoming, Agents: agents, AgentID: agentID, Planned: planned,
		Prev: month.AddDate(0, -1, 0).Format("2006-01"), Next: month.AddDate(0, 1, 0).Format("2006-01")}
	s.render(w, r, "calendar", pageData{Title: "Calendar", Nav: "calendar", User: user, Data: data})
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
