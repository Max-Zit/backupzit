package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/backupzit/backupzit/internal/localipc"
)

// Summary is the interpreted agent status shown in the tray and window.
type Summary struct {
	State    string `json:"state"` // ok, running, warning, error, offline, idle
	Headline string `json:"headline"`
	Detail   string `json:"detail"`
	Percent  int    `json:"percent"` // -1 if unknown
	LastText string `json:"last_text"`
	NextText string `json:"next_text"`
	// Status is the raw status from the agent (nil if the service is not
	// reachable).
	Status *localipc.Status `json:"status"`
	Jobs   []JobView        `json:"jobs"`
	Error  string           `json:"error,omitempty"`
}

// JobView is a job with display texts.
type JobView struct {
	localipc.Job
	KindText string `json:"kind_text"`
	LastText string `json:"last_text"`
	NextText string `json:"next_text"`
}

var kindText = map[string]string{"files": "Files", "image": "Disk image", "vm": "Proxmox VMs", "copy": "Backup copy"}

var runKindText = map[string]string{
	"backup": "Backup", "image-backup": "Disk image backup", "vm-backup": "VM backup", "copy": "Backup copy",
	"restore": "Restore", "image-restore": "Disk restore", "image-file-restore": "File restore", "vm-restore": "VM restore",
	"verify": "Restore test", "agent-update": "Agent update",
}

// when renders a time relative to now: "today 14:05", "yesterday 22:00",
// "Mon 5 Oct 22:00".
func when(t time.Time, now time.Time) string {
	t = t.Local()
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.Local) }
	diff := int(day(y1, m1, d1).Sub(day(y2, m2, d2)).Hours() / 24)
	switch diff {
	case 0:
		return "today " + t.Format("15:04")
	case -1:
		return "yesterday " + t.Format("15:04")
	case 1:
		return "tomorrow " + t.Format("15:04")
	}
	return t.Format("Mon 2 Jan 15:04")
}

func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func summarize(st *localipc.Status, callErr error, now time.Time) Summary {
	s := Summary{Percent: -1, Jobs: []JobView{}}
	if st == nil {
		s.State, s.Headline = "offline", "Agent service is not running"
		s.Detail = "Start the \"BackupZit Agent\" service or reinstall the agent."
		if callErr != nil {
			s.Error = callErr.Error()
		}
		return s
	}
	s.Status = st

	// Newest finished backup and next scheduled run over all jobs.
	var lastJob *localipc.Job
	var next *time.Time
	failed, warned := 0, 0
	for i := range st.Jobs {
		j := st.Jobs[i]
		v := JobView{Job: j, KindText: kindText[j.Kind]}
		if v.KindText == "" {
			v.KindText = j.Kind
		}
		if j.LastFinished != nil {
			v.LastText = when(*j.LastFinished, now)
			if lastJob == nil || j.LastFinished.After(*lastJob.LastFinished) {
				lastJob = &st.Jobs[i]
			}
		} else {
			v.LastText = "never"
		}
		switch j.LastStatus {
		case "failed":
			failed++
		case "warning":
			warned++
		}
		if j.NextRun != nil {
			v.NextText = when(*j.NextRun, now)
			if next == nil || j.NextRun.Before(*next) {
				next = j.NextRun
			}
		} else if !j.Enabled {
			v.NextText = "disabled"
		} else {
			v.NextText = "manual only"
		}
		s.Jobs = append(s.Jobs, v)
	}
	if lastJob != nil {
		s.LastText = fmt.Sprintf("%s — %s, %s", when(*lastJob.LastFinished, now), lastJob.Name, lastJob.LastStatus)
	} else {
		s.LastText = "no backup yet"
	}
	if next != nil {
		s.NextText = when(*next, now)
	} else {
		s.NextText = "not scheduled"
	}

	switch {
	case st.Current != nil:
		c := st.Current
		s.State = "running"
		name := runKindText[c.Kind]
		if name == "" {
			name = c.Kind
		}
		if c.Job != "" {
			name += " — " + c.Job
		}
		s.Headline = name
		switch {
		case c.Total > 0:
			s.Percent = int(c.Done * 100 / c.Total)
			s.Detail = fmt.Sprintf("%d%% · %s of %s read · started %s", s.Percent, humanBytes(c.Done), humanBytes(c.Total), when(c.Started, now))
		case c.Files > 0 && c.Done > 0:
			s.Detail = fmt.Sprintf("%d files · %s read · started %s", c.Files, humanBytes(c.Done), when(c.Started, now))
		case c.Files > 0:
			s.Detail = fmt.Sprintf("%d files checked · started %s", c.Files, when(c.Started, now))
		default:
			s.Detail = "started " + when(c.Started, now)
		}
	case !st.Connected && st.LastContact == nil:
		s.State, s.Headline = "offline", "Not connected to the console"
		s.Detail = strings.TrimSpace("Trying to reach " + st.ServerURL + ". " + st.LastError)
	case len(st.Jobs) == 0 && st.JobsAt != nil:
		s.State, s.Headline = "idle", "No backup jobs for this computer"
		s.Detail = "Create a backup job for " + st.Hostname + " in the BackupZit console."
	case failed > 0:
		s.State, s.Headline = "error", "Last backup failed"
		if failed > 1 {
			s.Headline = fmt.Sprintf("%d backup jobs failed", failed)
		}
		s.Detail = "Open the console for details."
		for _, j := range st.Jobs {
			if j.LastStatus == "failed" && j.LastMessage != "" {
				s.Detail = j.Name + ": " + j.LastMessage
				break
			}
		}
	case warned > 0:
		s.State, s.Headline = "warning", "Backed up with warnings"
		s.Detail = "Some files could not be read. Open the console for the list."
	case lastJob != nil:
		s.State, s.Headline = "ok", "This computer is protected"
		s.Detail = "Last backup " + when(*lastJob.LastFinished, now) + "."
	default:
		s.State, s.Headline = "idle", "Waiting for the first backup"
		s.Detail = "Next backup " + s.NextText + "."
	}
	if !st.Connected && st.LastContact != nil && st.Current == nil {
		s.State = "offline"
		s.Detail = "Console not reachable since " + when(*st.LastContact, now) + ". Scheduled backups start once it is back. " + s.Detail
	}
	return s
}

// tooltip is the short text shown on hover (Windows limits it to 127
// characters).
func (s Summary) tooltip() string {
	t := "BackupZit — " + s.Headline
	if s.State == "running" && s.Percent >= 0 {
		t += fmt.Sprintf(" (%d%%)", s.Percent)
	} else if s.Status != nil && s.LastText != "" {
		t += "\nLast: " + s.LastText
	}
	if len(t) > 127 {
		t = t[:124] + "..."
	}
	return t
}
