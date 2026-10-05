package server

import (
	"context"
	"fmt"
	"html/template"
	"math"
	"strings"
	"time"
)

// Storage usage: after each backup the agent reports how much space its
// repository takes and, when the storage can tell, the capacity. The
// Storage page shows the history per target, the growth and a forecast.

// UsagePoint is the space used on one day.
type UsagePoint struct {
	Day   time.Time
	Bytes uint64
}

// TargetUsage is the space a storage target's repositories take.
type TargetUsage struct {
	Used  uint64 // newest size of each repository, summed
	Repos int
	// Total and Free are the capacity from the newest report, if known.
	Total, Free uint64
	Days        []UsagePoint // daily, oldest first, up to usageDays
	// GrowthPerDay is the trend over the last 30 days (bytes/day); Trend is
	// false when there is too little history.
	GrowthPerDay float64
	Trend        bool
	// FullInDays is when the storage is full at the current growth, -1 if
	// unknown or not growing.
	FullInDays int
	In90       uint64 // projected use in 90 days
}

const usageDays = 90

// UsedPercent is the share of the capacity in use.
func (u *TargetUsage) UsedPercent() int {
	if u.Total == 0 {
		return 0
	}
	return int(math.Round(float64(u.Total-u.Free) * 100 / float64(u.Total)))
}

// storageUsage computes the usage of every target with reports.
func (s *Store) storageUsage(ctx context.Context, now time.Time) (map[int64]*TargetUsage, error) {
	rows, err := s.db.Query(ctx, `SELECT target_id, repo_url, finished_at, repo_bytes, storage_total, storage_free FROM runs
		WHERE repo_bytes IS NOT NULL AND target_id IS NOT NULL AND finished_at IS NOT NULL ORDER BY finished_at`)
	if err != nil {
		return nil, err
	}
	type report struct {
		repo        string
		at          time.Time
		bytes       int64
		total, free *int64
	}
	byTarget := map[int64][]report{}
	for rows.Next() {
		var t int64
		var r report
		if err := rows.Scan(&t, &r.repo, &r.at, &r.bytes, &r.total, &r.free); err != nil {
			rows.Close()
			return nil, err
		}
		byTarget[t] = append(byTarget[t], r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	today := midnight(now.Local())
	out := map[int64]*TargetUsage{}
	for t, reps := range byTarget {
		u := &TargetUsage{FullInDays: -1}
		latest := map[string]int64{}
		start := midnight(reps[0].at.Local())
		if earliest := today.AddDate(0, 0, -(usageDays - 1)); start.Before(earliest) {
			start = earliest
		}
		i := 0
		for day := start; !day.After(today); day = day.AddDate(0, 0, 1) {
			end := day.AddDate(0, 0, 1)
			for ; i < len(reps) && reps[i].at.Before(end); i++ {
				latest[reps[i].repo] = reps[i].bytes
				if reps[i].total != nil && *reps[i].total > 0 {
					u.Total, u.Free = uint64(*reps[i].total), uint64(*reps[i].free)
				}
			}
			var sum uint64
			for _, b := range latest {
				sum += uint64(b)
			}
			u.Days = append(u.Days, UsagePoint{day, sum})
		}
		u.Repos = len(latest)
		u.Used = u.Days[len(u.Days)-1].Bytes
		u.GrowthPerDay, u.Trend = growth(u.Days, 30)
		if u.Trend {
			proj := float64(u.Used) + u.GrowthPerDay*90
			if proj < 0 {
				proj = 0
			}
			u.In90 = uint64(proj)
			if u.Total > 0 && u.GrowthPerDay > 0 {
				u.FullInDays = int(float64(u.Free) / u.GrowthPerDay)
			}
		}
		out[t] = u
	}
	return out, nil
}

// growth fits a line through the last n days with data (least squares) and
// returns its slope in bytes per day. It needs at least a week of history.
func growth(days []UsagePoint, n int) (float64, bool) {
	var pts []UsagePoint
	for _, d := range days {
		if d.Bytes > 0 {
			pts = append(pts, d)
		}
	}
	if len(pts) > n {
		pts = pts[len(pts)-n:]
	}
	if len(pts) < 7 {
		return 0, false
	}
	var sx, sy, sxx, sxy float64
	x0 := pts[0].Day
	for _, p := range pts {
		x := p.Day.Sub(x0).Hours() / 24
		y := float64(p.Bytes)
		sx, sy, sxx, sxy = sx+x, sy+y, sxx+x*x, sxy+x*y
	}
	k := float64(len(pts))
	den := k*sxx - sx*sx
	if den == 0 {
		return 0, false
	}
	return (k*sxy - sx*sy) / den, true
}

// storageWarning says how full a target is and when it fills up.
func storageWarning(l *language, u *TargetUsage) string {
	msg := l.T("storage %d %% full", u.UsedPercent())
	if u.FullInDays >= 0 {
		msg += l.T(", full in about %d days at the current growth", u.FullInDays)
	}
	return msg
}

// usageChart draws the history and, with a trend, the next 30 days as an
// SVG. Colours come from the stylesheet.
func usageChart(l *language, u *TargetUsage) template.HTML {
	if u == nil || len(u.Days) < 2 {
		return ""
	}
	const w, h, left, bottom, top = 640.0, 160.0, 64.0, 22.0, 8.0
	future := 0
	if u.Trend {
		future = 30
	}
	n := len(u.Days) - 1 + future
	ymax := float64(u.Total)
	for _, d := range u.Days {
		ymax = math.Max(ymax, float64(d.Bytes))
	}
	proj := float64(u.Used) + u.GrowthPerDay*float64(future)
	if u.Total == 0 {
		ymax = math.Max(ymax, proj)
	}
	if ymax == 0 {
		return ""
	}
	ymax *= 1.08
	x := func(i int) float64 { return left + (w-left-4)*float64(i)/float64(n) }
	y := func(b float64) float64 { return top + (h-top-bottom)*(1-b/ymax) }
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="usage-chart" viewBox="0 0 %.0f %.0f" role="img" aria-label="%s">`, w, h, template.HTMLEscapeString(l.T("Storage usage")))
	for _, f := range []float64{0, 0.5, 1} {
		v := ymax / 1.08 * f
		fmt.Fprintf(&b, `<line class="uc-grid" x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f"/>`, left, w-4, y(v), y(v))
		fmt.Fprintf(&b, `<text class="uc-text" x="%.1f" y="%.1f" text-anchor="end">%s</text>`, left-6, y(v)+4, humanBytes(uint64(v)))
	}
	if u.Total > 0 {
		fmt.Fprintf(&b, `<line class="uc-cap" x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f"/>`, left, w-4, y(float64(u.Total)), y(float64(u.Total)))
	}
	var line strings.Builder
	for i, d := range u.Days {
		fmt.Fprintf(&line, "%.1f,%.1f ", x(i), y(float64(d.Bytes)))
	}
	last := len(u.Days) - 1
	fmt.Fprintf(&b, `<polygon class="uc-area" points="%.1f,%.1f %s%.1f,%.1f"/>`, x(0), y(0), line.String(), x(last), y(0))
	fmt.Fprintf(&b, `<polyline class="uc-line" points="%s"/>`, strings.TrimSpace(line.String()))
	if future > 0 {
		pv := math.Max(proj, 0)
		if u.Total > 0 {
			pv = math.Min(pv, float64(u.Total))
		}
		fmt.Fprintf(&b, `<line class="uc-proj" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/>`, x(last), y(float64(u.Used)), x(n), y(pv))
	}
	dayFormat := "Jan 2"
	if l.Code == "sr" {
		dayFormat = "2.1."
	}
	label := func(i int, t time.Time, anchor string) {
		fmt.Fprintf(&b, `<text class="uc-text" x="%.1f" y="%.1f" text-anchor="%s">%s</text>`, x(i), h-6, anchor, t.Format(dayFormat))
	}
	label(0, u.Days[0].Day, "start")
	label(last, u.Days[last].Day, map[bool]string{true: "middle", false: "end"}[future > 0])
	if future > 0 {
		label(n, u.Days[last].Day.AddDate(0, 0, future), "end")
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}
