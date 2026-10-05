package server

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/repo"
)

// The console speaks English and Serbian. Templates mark visible text with
// {{T "English text"}}; a catalog per language maps the English text to the
// translation (tools/i18n lists the texts and the missing translations).
// Untranslated texts stay English.

//go:embed i18n/*.json
var catalogFS embed.FS

// language is a console language.
type language struct {
	Code, Name string
	msgs       map[string]string
}

var languages = []*language{{Code: "en", Name: "English"}, {Code: "sr", Name: "Srpski"}}

const langCookie = "bz_lang"

func init() {
	for _, l := range languages {
		b, err := catalogFS.ReadFile("i18n/" + l.Code + ".json")
		if err != nil {
			continue
		}
		if err := json.Unmarshal(b, &l.msgs); err != nil {
			panic(fmt.Sprintf("i18n/%s.json: %v", l.Code, err))
		}
	}
}

// T translates an English text (a fmt format when args are given). A
// translation may hold plural forms separated by "|"; the first argument
// chooses one (see pluralForm).
func (l *language) T(s string, args ...any) string {
	if t, ok := l.msgs[s]; ok && t != "" {
		s = t
		if strings.Contains(s, "|") && len(args) > 0 {
			if n, ok := args[0].(int); ok {
				s = pluralForm(l.Code, n, strings.Split(s, "|"))
			}
		}
	}
	if len(args) > 0 {
		return fmt.Sprintf(s, args...)
	}
	return s
}

// N is a noun for a count: the English singular or plural, or the
// language's plural form of the plural's translation.
func (l *language) N(n int, singular, plural string) string {
	if t, ok := l.msgs[plural]; ok && strings.Contains(t, "|") {
		return pluralForm(l.Code, n, strings.Split(t, "|"))
	}
	if n == 1 {
		return l.T(singular)
	}
	return l.T(plural)
}

// pluralForm picks one of the plural forms of a translation. Serbian has
// three: 1, 21, 31 … / 2–4, 22–24 … / the rest.
func pluralForm(code string, n int, forms []string) string {
	i := 1
	if n == 1 {
		i = 0
	}
	if code == "sr" {
		switch m10, m100 := n%10, n%100; {
		case m10 == 1 && m100 != 11:
			i = 0
		case m10 >= 2 && m10 <= 4 && (m100 < 12 || m100 > 14):
			i = 1
		default:
			i = 2
		}
	}
	if i >= len(forms) {
		i = len(forms) - 1
	}
	return forms[i]
}

func languageByCode(code string) *language {
	for _, l := range languages {
		if l.Code == code {
			return l
		}
	}
	return nil
}

// requestLanguage is the language chosen with the switch in the console
// (cookie), else the browser's preference, else English.
func requestLanguage(r *http.Request) *language {
	if c, err := r.Cookie(langCookie); err == nil {
		if l := languageByCode(c.Value); l != nil {
			return l
		}
	}
	for _, part := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		tag := strings.ToLower(strings.TrimSpace(strings.SplitN(part, ";", 2)[0]))
		switch {
		case strings.HasPrefix(tag, "sr"), strings.HasPrefix(tag, "hr"), strings.HasPrefix(tag, "bs"), strings.HasPrefix(tag, "sh"), strings.HasPrefix(tag, "cnr"):
			return languageByCode("sr")
		case strings.HasPrefix(tag, "en"):
			return languages[0]
		}
	}
	return languages[0]
}

// langFuncs are the template functions of a language: T, and the helpers
// whose output is text.
func langFuncs(l *language) template.FuncMap {
	return template.FuncMap{
		"T":           l.T,
		"N":           l.N,
		"lang":        func() string { return l.Code },
		"languages":   func() []*language { return languages },
		"ago":         func(t any) string { return agoText(l, t) },
		"schedule":    func(stored string) string { return describeSchedule(l, stored) },
		"nextrun":     func(stored string) string { return nextRunText(l, stored) },
		"kindtitle":   func(k string) string { return l.T(kindTitleEN(k)) },
		"jobkind":     func(k string) string { return l.T(jobKindTitle(k)) },
		"coverage":    func(c *JobCoverage) string { return c.SummaryIn(l) },
		"retention":   func(p repo.RetentionPolicy) string { return retentionText(l, p) },
		"minutesText": func(m int) string { return minutesTextIn(l, m) },
		"usagechart":  func(u *TargetUsage) template.HTML { return usageChart(l, u) },
		"reportsched": func(rs ReportSchedule) string { return rs.DescribeIn(l) },
		"vmsel":       func(paths []string) string { return describeVMSelection(l, paths) },
		"imagesel":    func(disk *int, parts []int) string { return describeImageSelection(l, disk, parts) },
	}
}

// retentionText describes a retention policy, e.g. "keep 7 daily and 4 weekly".
func retentionText(l *language, p repo.RetentionPolicy) string {
	if p.Empty() {
		return l.T("keep all backups")
	}
	var s []string
	add := func(n int, format string) {
		if n > 0 {
			s = append(s, l.T(format, n))
		}
	}
	add(p.KeepLast, "%d most recent")
	add(p.KeepDaily, "%d daily")
	add(p.KeepWeekly, "%d weekly")
	add(p.KeepMonthly, "%d monthly")
	list := s[len(s)-1]
	if len(s) > 1 {
		list = l.T("%s and %s", strings.Join(s[:len(s)-1], ", "), list)
	}
	return l.T("keep %s", list)
}

// agoText says how long ago t was.
func agoText(l *language, t any) string {
	var tm time.Time
	switch v := t.(type) {
	case time.Time:
		tm = v
	case *time.Time:
		if v == nil {
			return l.T("never")
		}
		tm = *v
	default:
		return ""
	}
	d := time.Since(tm)
	switch {
	case d < time.Minute:
		return l.T("just now")
	case d < time.Hour:
		return l.T("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return l.T("%d h ago", int(d.Hours()))
	}
	return l.T("%d days ago", int(d.Hours()/24))
}

// handleLanguage serves POST /lang: remembers the language in a cookie and
// returns to the page.
func (s *Server) handleLanguage(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && !sameOrigin(r) {
		http.Error(w, "cross-site request rejected", http.StatusForbidden)
		return
	}
	if l := languageByCode(r.FormValue("lang")); l != nil {
		http.SetCookie(w, &http.Cookie{Name: langCookie, Value: l.Code, Path: "/", MaxAge: 5 * 365 * 24 * 3600, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil})
	}
	back := r.FormValue("back")
	if back == "" {
		// Return to the page the switch was on.
		if u, err := url.Parse(r.Referer()); err == nil && u.Path != "" {
			back = u.Path
			if u.RawQuery != "" && !strings.Contains(u.RawQuery, "sig=") {
				back += "?" + u.RawQuery
			}
		}
	}
	if !strings.HasPrefix(back, "/") || strings.HasPrefix(back, "//") {
		back = "/"
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}
