package server

import (
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Every translated guide page keeps the anchors, links and template calls
// of the English page, so links into the guide work in every language.
func TestDocTranslationsMatchEnglish(t *testing.T) {
	ids := regexp.MustCompile(`\bid="([^"]+)"`)
	links := regexp.MustCompile(`href="(/[^"]*)"`)
	calls := regexp.MustCompile(`\{\{[^}]*\}\}`)
	set := func(re *regexp.Regexp, s string) string {
		var out []string
		for _, m := range re.FindAllStringSubmatch(s, -1) {
			out = append(out, m[len(m)-1])
		}
		sort.Strings(out)
		return strings.Join(out, " ")
	}
	langs, _ := fs.ReadDir(templateFS, "templates/docs")
	for _, l := range langs {
		files, _ := fs.ReadDir(templateFS, "templates/docs/"+l.Name())
		for _, f := range files {
			tr, _ := fs.ReadFile(templateFS, path.Join("templates/docs", l.Name(), f.Name()))
			en, err := fs.ReadFile(templateFS, "templates/"+f.Name())
			if err != nil {
				t.Errorf("%s/%s has no English page", l.Name(), f.Name())
				continue
			}
			for _, c := range []struct {
				what string
				re   *regexp.Regexp
			}{{"anchors", ids}, {"links", links}, {"template calls", calls}} {
				if a, b := set(c.re, string(en)), set(c.re, string(tr)); a != b {
					t.Errorf("%s/%s: %s differ from English:\n en: %s\n %s: %s", l.Name(), f.Name(), c.what, a, l.Name(), b)
				}
			}
		}
	}
}

// Every console language has every guide page, so no language falls back
// to English for a page.
func TestDocTranslationsComplete(t *testing.T) {
	pages, _ := fs.Glob(templateFS, "templates/doc_*.html")
	if len(pages) == 0 {
		t.Fatal("no English guide pages")
	}
	for _, l := range languages[1:] {
		for _, p := range pages {
			name := path.Base(p)
			if _, err := fs.Stat(templateFS, path.Join("templates/docs", l.Code, name)); err != nil {
				t.Errorf("%s has no translation of %s", l.Code, name)
			}
		}
	}
}
