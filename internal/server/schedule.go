package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Schedule kinds, as offered in the console.
const (
	SchedManual   = "manual"
	SchedDaily    = "daily"    // at fixed times on selected weekdays
	SchedInterval = "interval" // every N minutes, optionally within a time window
	SchedMonthly  = "monthly"  // on a day of the month at a fixed time
	SchedAfter    = "after"    // copy jobs: after each backup of the source job
)

// Schedule is a human-friendly backup schedule. It is stored as JSON in
// jobs.schedule and converted to cron expressions internally.
type Schedule struct {
	Kind string `json:"kind"`
	// Times are "HH:MM" (daily: all of them; monthly: the first).
	Times []string `json:"times,omitempty"`
	// Days are weekdays 0=Sunday..6=Saturday; empty means every day.
	Days []int `json:"days,omitempty"`
	// EveryMinutes is the interval for SchedInterval (15, 30, 60, 120, ...).
	EveryMinutes int `json:"every_minutes,omitempty"`
	// From/To limit an interval schedule to a window of whole hours
	// (From inclusive, To exclusive). Both zero means all day.
	FromHour int `json:"from_hour,omitempty"`
	ToHour   int `json:"to_hour,omitempty"`
	// DayOfMonth for SchedMonthly (1..28).
	DayOfMonth int `json:"day_of_month,omitempty"`

	// cron holds a raw cron expression for schedules created before the
	// friendly format existed.
	cron string
}

// ParseSchedule decodes a stored schedule. Empty means manual; a value
// that is not JSON is treated as a cron expression.
func ParseSchedule(s string) (Schedule, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Schedule{Kind: SchedManual}, nil
	}
	if !strings.HasPrefix(s, "{") {
		if _, err := cronParser.Parse(s); err != nil {
			return Schedule{}, fmt.Errorf("invalid schedule %q: %w", s, err)
		}
		if sc, ok := dailyFromCron(s); ok {
			return sc, nil
		}
		return Schedule{Kind: "cron", cron: s}, nil
	}
	var sc Schedule
	if err := json.Unmarshal([]byte(s), &sc); err != nil {
		return Schedule{}, fmt.Errorf("invalid schedule: %w", err)
	}
	return sc, sc.Validate()
}

// dailyFromCron converts "M H * * DOW" (DOW "*" or a list of 0-6) from
// older jobs into the friendly daily form.
func dailyFromCron(expr string) (Schedule, bool) {
	f := strings.Fields(expr)
	if len(f) != 5 || f[2] != "*" || f[3] != "*" {
		return Schedule{}, false
	}
	m, err1 := strconv.Atoi(f[0])
	h, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil || m < 0 || m > 59 || h < 0 || h > 23 {
		return Schedule{}, false
	}
	sc := Schedule{Kind: SchedDaily, Times: []string{fmt.Sprintf("%02d:%02d", h, m)}}
	if f[4] != "*" {
		for _, d := range strings.Split(f[4], ",") {
			n, err := strconv.Atoi(d)
			if err != nil || n < 0 || n > 6 {
				return Schedule{}, false
			}
			sc.Days = append(sc.Days, n)
		}
	}
	return sc, sc.Validate() == nil
}

// Encode returns the stored form ("" for manual).
func (s Schedule) Encode() string {
	if s.cron != "" {
		return s.cron
	}
	if s.Kind == SchedManual || s.Kind == "" {
		return ""
	}
	b, _ := json.Marshal(s)
	return string(b)
}

func parseHM(v string) (h, m int, err error) {
	hs, ms, ok := strings.Cut(strings.TrimSpace(v), ":")
	if ok {
		h, err = strconv.Atoi(hs)
		if err == nil {
			m, err = strconv.Atoi(ms)
		}
	}
	if !ok || err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("invalid time %q (use HH:MM)", v)
	}
	return h, m, nil
}

// Validate checks the schedule and normalizes days and times.
func (s *Schedule) Validate() error {
	for _, d := range s.Days {
		if d < 0 || d > 6 {
			return fmt.Errorf("invalid weekday %d", d)
		}
	}
	sort.Ints(s.Days)
	if len(s.Days) == 7 {
		s.Days = nil
	}
	switch s.Kind {
	case SchedManual:
		return nil
	case SchedAfter:
		s.Days, s.Times = nil, nil
		return nil
	case SchedDaily:
		if len(s.Times) == 0 {
			return errors.New("choose at least one time")
		}
		seen := map[string]bool{}
		var times []string
		for _, t := range s.Times {
			h, m, err := parseHM(t)
			if err != nil {
				return err
			}
			n := fmt.Sprintf("%02d:%02d", h, m)
			if !seen[n] {
				seen[n] = true
				times = append(times, n)
			}
		}
		sort.Strings(times)
		s.Times = times
	case SchedInterval:
		if s.EveryMinutes <= 0 || (s.EveryMinutes < 60 && 60%s.EveryMinutes != 0) ||
			(s.EveryMinutes >= 60 && (s.EveryMinutes%60 != 0 || 24%(s.EveryMinutes/60) != 0)) {
			return fmt.Errorf("unsupported interval of %d minutes", s.EveryMinutes)
		}
		if s.FromHour < 0 || s.FromHour > 23 || s.ToHour < 0 || s.ToHour > 24 {
			return errors.New("invalid time window")
		}
		if s.FromHour != 0 || s.ToHour != 0 {
			if s.ToHour <= s.FromHour {
				return errors.New("the time window must end after it starts")
			}
		}
	case SchedMonthly:
		if s.DayOfMonth < 1 || s.DayOfMonth > 28 {
			return errors.New("day of month must be between 1 and 28")
		}
		if len(s.Times) == 0 {
			return errors.New("choose a time")
		}
		h, m, err := parseHM(s.Times[0])
		if err != nil {
			return err
		}
		s.Times = []string{fmt.Sprintf("%02d:%02d", h, m)}
		s.Days = nil
	default:
		return fmt.Errorf("unknown schedule type %q", s.Kind)
	}
	return nil
}

func (s Schedule) dowField() string {
	if len(s.Days) == 0 {
		return "*"
	}
	parts := make([]string, len(s.Days))
	for i, d := range s.Days {
		parts[i] = strconv.Itoa(d)
	}
	return strings.Join(parts, ",")
}

// crons converts the schedule to cron expressions (one per distinct time).
func (s Schedule) crons() []string {
	if s.cron != "" {
		return []string{s.cron}
	}
	switch s.Kind {
	case SchedDaily:
		out := make([]string, 0, len(s.Times))
		for _, t := range s.Times {
			h, m, _ := parseHM(t)
			out = append(out, fmt.Sprintf("%d %d * * %s", m, h, s.dowField()))
		}
		return out
	case SchedInterval:
		hours := "*"
		if s.FromHour != 0 || s.ToHour != 0 {
			hours = fmt.Sprintf("%d-%d", s.FromHour, s.ToHour-1)
		}
		if s.EveryMinutes < 60 {
			return []string{fmt.Sprintf("*/%d %s * * %s", s.EveryMinutes, hours, s.dowField())}
		}
		step := s.EveryMinutes / 60
		if hours == "*" {
			hours = "0-23"
		}
		return []string{fmt.Sprintf("0 %s/%d * * %s", hours, step, s.dowField())}
	case SchedMonthly:
		h, m, _ := parseHM(s.Times[0])
		return []string{fmt.Sprintf("%d %d %d * *", m, h, s.DayOfMonth)}
	}
	return nil
}

// Next returns the first run time after t, or zero for manual schedules.
func (s Schedule) Next(t time.Time) time.Time {
	var next time.Time
	for _, expr := range s.crons() {
		c, err := cronParser.Parse(expr)
		if err != nil {
			continue
		}
		if n := c.Next(t); next.IsZero() || n.Before(next) {
			next = n
		}
	}
	return next
}

var dayNames = []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}

func (s Schedule) daysText(l *language) string {
	switch d := s.dowField(); d {
	case "*":
		return l.T("every day")
	case "1,2,3,4,5":
		return l.T("Mon–Fri")
	case "0,6":
		return l.T("Sat and Sun")
	}
	names := make([]string, len(s.Days))
	for i, d := range s.Days {
		names[i] = l.T(dayNames[d])
	}
	return l.T("on %s", strings.Join(names, ", "))
}

// Describe returns a human readable summary, e.g. "Every day at 22:00".
func (s Schedule) Describe() string { return s.DescribeIn(languages[0]) }

// DescribeIn is Describe in a console language.
func (s Schedule) DescribeIn(l *language) string {
	if s.cron != "" {
		return "cron: " + s.cron
	}
	switch s.Kind {
	case SchedDaily:
		d := []rune(s.daysText(l))
		return l.T("%s at %s", strings.ToUpper(string(d[:1]))+string(d[1:]), strings.Join(s.Times, ", "))
	case SchedInterval:
		var every string
		switch {
		case s.EveryMinutes < 60:
			every = l.T("Every %d minutes", s.EveryMinutes)
		case s.EveryMinutes == 60:
			every = l.T("Every hour")
		default:
			every = l.T("Every %d hours", s.EveryMinutes/60)
		}
		if s.FromHour != 0 || s.ToHour != 0 {
			every = l.T("%s between %02d:00 and %02d:00", every, s.FromHour, s.ToHour)
		}
		return every + ", " + s.daysText(l)
	case SchedMonthly:
		return l.T("Monthly on day %d at %s", s.DayOfMonth, s.Times[0])
	case SchedAfter:
		return l.T("After each backup of the source job")
	}
	return l.T("Manual only")
}

// IsManual reports whether the job only runs when started by hand.
func (s Schedule) IsManual() bool { return s.cron == "" && (s.Kind == SchedManual || s.Kind == "") }
