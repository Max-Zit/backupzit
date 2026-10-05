package server

import (
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/repo"
)

func TestScheduleInSerbian(t *testing.T) {
	sr := languageByCode("sr")
	for stored, want := range map[string]string{
		"0 22 * * *":         "Svaki dan u 22:00",
		"0 22 * * 1,2,3,4,5": "Pon–pet u 22:00",
		"0 3 * * 6":          "Sub u 03:00",
	} {
		if got := describeSchedule(sr, stored); got != want {
			t.Errorf("%s: %q, want %q", stored, got, want)
		}
	}
	if got := agoText(sr, time.Now().Add(-3*24*time.Hour)); got != "pre 3 dana" {
		t.Errorf("ago: %q", got)
	}
	if got := describeVMSelection(sr, []string{"*"}); got != "Svi VM-ovi (i kontejneri) na hostu" {
		t.Errorf("vm selection: %q", got)
	}
}

func TestRequestLanguage(t *testing.T) {
	for header, want := range map[string]string{"": "en", "sr-Latn-RS,sr;q=0.9": "sr", "hr-HR": "sr", "de-DE,en;q=0.8": "de", "fr-CA": "fr", "zh-CN,zh;q=0.9": "zh", "sk": "cs", "ko-KR,ko;q=0.9": "en"} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Accept-Language", header)
		if got := requestLanguage(r).Code; got != want {
			t.Errorf("%q: %s, want %s", header, got, want)
		}
	}
}

func TestSerbianPlurals(t *testing.T) {
	sr := languageByCode("sr")
	for n, want := range map[int]string{1: "tačka vraćanja", 3: "tačke vraćanja", 5: "tačaka vraćanja", 12: "tačaka vraćanja", 21: "tačka vraćanja", 22: "tačke vraćanja"} {
		if got := sr.N(n, "restore point", "restore points"); got != want {
			t.Errorf("%d: %q, want %q", n, got, want)
		}
	}
	if got := languages[0].N(2, "restore point", "restore points"); got != "restore points" {
		t.Errorf("en: %q", got)
	}
	every := Schedule{Kind: SchedInterval, EveryMinutes: 120, FromHour: 8, ToHour: 18, Days: []int{1, 2, 3, 4, 5}}
	if got := every.DescribeIn(sr); got != "Na svaka 2 sata između 08:00 i 18:00, pon–pet" {
		t.Errorf("interval: %q", got)
	}
	if got := (JobCoverage{Unit: "database", Items: []string{"a", "b"}}).SummaryIn(sr); got != "2 baze: a, b" {
		t.Errorf("coverage: %q", got)
	}
}

func TestRetentionText(t *testing.T) {
	p := repo.RetentionPolicy{KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 12}
	if got := retentionText(languages[0], p); got != p.String() {
		t.Errorf("en: %q, want %q", got, p.String())
	}
	if got := retentionText(languageByCode("sr"), p); got != "čuvaj 7 dnevnih, 4 nedeljna i 12 mesečnih" {
		t.Errorf("sr: %q", got)
	}
}

func TestShorterRetention(t *testing.T) {
	all := repo.RetentionPolicy{}
	p := repo.RetentionPolicy{KeepDaily: 7, KeepWeekly: 4}
	for _, c := range []struct {
		old, cur repo.RetentionPolicy
		want     bool
	}{
		{all, p, true},
		{p, all, false},
		{p, repo.RetentionPolicy{KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 12}, false},
		{p, repo.RetentionPolicy{KeepDaily: 3, KeepWeekly: 4}, true},
		{p, repo.RetentionPolicy{KeepDaily: 14, KeepWeekly: 8}, false},
	} {
		if got := shorterRetention(c.old, c.cur); got != c.want {
			t.Errorf("%v → %v: %v", c.old, c.cur, got)
		}
	}
}

// TestCatalogs keeps every language in step with the Serbian catalog: the
// same texts, the same format verbs and as many plural forms as the
// language's plural rule picks from.
func TestCatalogs(t *testing.T) {
	verbs := regexp.MustCompile(`%(\[\d+\])?[-+# 0]*\d*(\.\d+)?[a-z%]`)
	sorted := func(s string) string {
		v := verbs.FindAllString(s, -1)
		sort.Strings(v)
		return strings.Join(v, " ")
	}
	sr := languageByCode("sr")
	for _, l := range languages {
		if l.Code == "en" || l.Code == "sr" {
			continue
		}
		want := map[string]int{"zh": 1, "ja": 1, "pl": 3, "cs": 3, "ru": 3}[l.Code]
		if want == 0 {
			want = 2
		}
		for k, s := range sr.msgs {
			v, ok := l.msgs[k]
			if !ok || v == "" {
				t.Errorf("%s: %q not translated", l.Code, k)
				continue
			}
			if strings.Contains(s, "|") {
				if n := len(strings.Split(v, "|")); n != want {
					t.Errorf("%s: %q has %d plural forms, want %d", l.Code, k, n, want)
				}
				for _, f := range strings.Split(v, "|") {
					if sorted(f) != sorted(strings.Split(s, "|")[0]) {
						t.Errorf("%s: %q form %q: format verbs differ", l.Code, k, f)
					}
				}
			} else if sorted(v) != sorted(k) {
				t.Errorf("%s: %q => %q: format verbs differ", l.Code, k, v)
			}
		}
		for k := range l.msgs {
			if _, ok := sr.msgs[k]; !ok {
				t.Errorf("%s: %q is not in the Serbian catalog", l.Code, k)
			}
		}
	}
}
