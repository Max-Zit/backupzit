package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestScheduleNextAndDescribe(t *testing.T) {
	loc := time.Local
	// Wednesday 2026-10-07 10:30 local time.
	now := time.Date(2026, 10, 7, 10, 30, 0, 0, loc)
	at := func(day, h, m int) time.Time { return time.Date(2026, 10, day, h, m, 0, 0, loc) }

	cases := []struct {
		name string
		s    Schedule
		next time.Time
		text string
	}{
		{"daily one time", Schedule{Kind: SchedDaily, Times: []string{"22:00"}},
			at(7, 22, 0), "Every day at 22:00"},
		{"daily several times, earliest next", Schedule{Kind: SchedDaily, Times: []string{"18:00", "8:15", "12:00"}},
			at(7, 12, 0), "Every day at 08:15, 12:00, 18:00"},
		{"workdays only, already past today", Schedule{Kind: SchedDaily, Times: []string{"07:00"}, Days: []int{5, 1, 2, 3, 4}},
			at(8, 7, 0), "Mon–Fri at 07:00"},
		{"weekend", Schedule{Kind: SchedDaily, Times: []string{"03:00"}, Days: []int{0, 6}},
			at(10, 3, 0), "Sat and Sun at 03:00"},
		{"all seven days collapse to every day", Schedule{Kind: SchedDaily, Times: []string{"01:00"}, Days: []int{0, 1, 2, 3, 4, 5, 6}},
			at(8, 1, 0), "Every day at 01:00"},
		{"every 4 hours", Schedule{Kind: SchedInterval, EveryMinutes: 240},
			at(7, 12, 0), "Every 4 hours, every day"},
		{"every 2 hours in office hours", Schedule{Kind: SchedInterval, EveryMinutes: 120, FromHour: 8, ToHour: 18, Days: []int{1, 2, 3, 4, 5}},
			at(7, 12, 0), "Every 2 hours between 08:00 and 18:00, Mon–Fri"},
		{"every 30 minutes", Schedule{Kind: SchedInterval, EveryMinutes: 30},
			at(7, 11, 0), "Every 30 minutes, every day"},
		{"window ends: next day", Schedule{Kind: SchedInterval, EveryMinutes: 60, FromHour: 6, ToHour: 9},
			at(8, 6, 0), "Every hour between 06:00 and 09:00, every day"},
		{"monthly", Schedule{Kind: SchedMonthly, DayOfMonth: 1, Times: []string{"23:30"}},
			time.Date(2026, 11, 1, 23, 30, 0, 0, loc), "Monthly on day 1 at 23:30"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := c.s
			if err := s.Validate(); err != nil {
				t.Fatal(err)
			}
			// Round trip through storage.
			back, err := ParseSchedule(s.Encode())
			if err != nil {
				t.Fatal(err)
			}
			if got := back.Next(now); !got.Equal(c.next) {
				t.Errorf("next: got %v want %v", got, c.next)
			}
			if got := back.Describe(); got != c.text {
				t.Errorf("describe: got %q want %q", got, c.text)
			}
		})
	}
}

func TestScheduleManualAndLegacy(t *testing.T) {
	m, err := ParseSchedule("")
	if err != nil || !m.IsManual() || !m.Next(time.Now()).IsZero() || m.Encode() != "" {
		t.Errorf("manual: %+v %v", m, err)
	}
	c, err := ParseSchedule("*/5 * * * *")
	if err != nil || c.Next(time.Now()).IsZero() || c.Encode() != "*/5 * * * *" {
		t.Errorf("legacy cron: %+v %v", c, err)
	}
	d, err := ParseSchedule("30 22 * * 1,2,3,4,5")
	if err != nil || d.Describe() != "Mon–Fri at 22:30" {
		t.Errorf("legacy daily cron: %q %v", d.Describe(), err)
	}
}

func TestScheduleValidation(t *testing.T) {
	bad := []Schedule{
		{Kind: SchedDaily},
		{Kind: SchedDaily, Times: []string{"25:00"}},
		{Kind: SchedDaily, Times: []string{"10:00"}, Days: []int{7}},
		{Kind: SchedInterval, EveryMinutes: 45},
		{Kind: SchedInterval, EveryMinutes: 300}, // 5 h does not divide the day
		{Kind: SchedInterval, EveryMinutes: 60, FromHour: 18, ToHour: 8},
		{Kind: SchedMonthly, DayOfMonth: 31, Times: []string{"10:00"}},
		{Kind: "weekly"},
	}
	for _, s := range bad {
		if err := s.Validate(); err == nil {
			t.Errorf("accepted invalid schedule %+v", s)
		}
	}
	if _, err := ParseSchedule("not a cron"); err == nil {
		t.Error("accepted garbage")
	}
}

func TestScheduleFromForm(t *testing.T) {
	form := url.Values{
		"sched_kind": {"daily"}, "sched_time": {"21:30", "", "06:00"},
		"sched_days": {"1", "2", "3", "4", "5"},
	}
	r, _ := http.NewRequest(http.MethodPost, "/jobs", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.ParseForm()
	stored, err := scheduleFromForm(r)
	if err != nil {
		t.Fatal(err)
	}
	if got := DescribeSchedule(stored); got != "Mon–Fri at 06:00, 21:30" {
		t.Errorf("got %q", got)
	}

	form = url.Values{"sched_kind": {"daily"}, "sched_time": {"21:30"}}
	r, _ = http.NewRequest(http.MethodPost, "/jobs", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.ParseForm()
	if _, err := scheduleFromForm(r); err == nil {
		t.Error("daily schedule without days accepted")
	}
}
