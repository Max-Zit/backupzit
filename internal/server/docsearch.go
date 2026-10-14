package server

import (
	"bytes"
	"html"
	"html/template"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// Search in the user guide: the guide's pages are split at their headings
// once, and a search returns the sections that contain every word.

type docSection struct {
	Slug, Page, Heading, Anchor string
	text, lower, lowerHead      string
}

type docHit struct {
	Slug, Page, Heading, Anchor string
	Snippet                     template.HTML
	score                       int
}

var (
	docIndexMu sync.Mutex
	docIndex   = map[string][]docSection{} // by language

	reNav     = regexp.MustCompile(`(?s)<nav class="docnav".*?</nav>`)
	reHeading = regexp.MustCompile(`(?s)<h([23])([^>]*)>(.*?)</h[23]>`)
	reID      = regexp.MustCompile(`\bid="([^"]+)"`)
	reTag     = regexp.MustCompile(`(?s)<[^>]+>`)
	reSpace   = regexp.MustCompile(`\s+`)
)

func plainText(s string) string {
	return strings.TrimSpace(reSpace.ReplaceAllString(html.UnescapeString(reTag.ReplaceAllString(s, " ")), " "))
}

// docSections renders the guide in a language (pages without a
// translation are English) and splits it at h2/h3 headings.
func (s *Server) docSections(lang string) []docSection {
	docIndexMu.Lock()
	defer docIndexMu.Unlock()
	if idx, ok := docIndex[lang]; ok {
		return idx
	}
	var idx []docSection
	pages := s.pages[lang]
	if pages == nil {
		pages = s.pages["en"]
	}
	{
		for _, p := range docPages {
			t := pages["doc_"+p.Slug]
			if t == nil {
				continue
			}
			var b bytes.Buffer
			if err := t.ExecuteTemplate(&b, "content", pageData{Data: map[string]any{"Page": p.Slug, "Pages": docPages, "Title": p.Title}}); err != nil {
				continue
			}
			body := reNav.ReplaceAllString(b.String(), "")
			cuts := reHeading.FindAllStringSubmatchIndex(body, -1)
			add := func(heading, anchor, part string) {
				text := plainText(part)
				if text == "" && heading == "" {
					return
				}
				idx = append(idx, docSection{Slug: p.Slug, Page: p.Title, Heading: heading, Anchor: anchor,
					text: text, lower: foldCase(text), lowerHead: foldCase(heading + " " + p.Title)})
			}
			start, heading, anchor := 0, "", ""
			for _, c := range cuts {
				add(heading, anchor, body[start:c[0]])
				heading = plainText(body[c[6]:c[7]])
				anchor = ""
				if m := reID.FindStringSubmatch(body[c[4]:c[5]]); m != nil {
					anchor = m[1]
				}
				start = c[1]
			}
			add(heading, anchor, body[start:])
		}
	}
	docIndex[lang] = idx
	return idx
}

// searchDocs finds the sections containing all words of q.
func (s *Server) searchDocs(lang, q string) []docHit {
	var terms []string
	for _, w := range strings.Fields(foldCase(q)) {
		if utf8.RuneCountInString(w) >= 2 && len(terms) < 8 {
			terms = append(terms, w)
		}
	}
	if len(terms) == 0 {
		return nil
	}
	var hits []docHit
	for _, sec := range s.docSections(lang) {
		score := 0
		for _, t := range terms {
			n := strings.Count(sec.lower, t) + 5*strings.Count(sec.lowerHead, t)
			if n == 0 {
				score = 0
				break
			}
			score += n
		}
		if score > 0 {
			hits = append(hits, docHit{Slug: sec.Slug, Page: sec.Page, Heading: sec.Heading, Anchor: sec.Anchor, Snippet: snippet(sec.text, sec.lower, terms), score: score})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	if len(hits) > 30 {
		hits = hits[:30]
	}
	return hits
}

// foldCase lowercases s letter by letter, so it has as many letters as s
// (strings.ToLower turns the Turkish İ into two). Search texts and terms
// use it, and snippet can cut both at the same letter.
func foldCase(s string) string { return strings.Map(unicode.ToLower, s) }

// snippet cuts about 220 letters around the first match and marks the
// words; everything else is escaped. lower is foldCase(text).
func snippet(text, lower string, terms []string) template.HTML {
	tr, lr := []rune(text), []rune(lower)
	if len(lr) != len(tr) {
		lr = []rune(foldCase(text))
	}
	rterms := make([][]rune, 0, len(terms))
	for _, t := range terms {
		rterms = append(rterms, []rune(t))
	}
	hasAt := func(i int, t []rune) bool {
		if i+len(t) > len(lr) {
			return false
		}
		for k, r := range t {
			if lr[i+k] != r {
				return false
			}
		}
		return true
	}
	first := -1
	for i := range lr {
		for _, t := range rterms {
			if hasAt(i, t) {
				first = i
				break
			}
		}
		if first >= 0 {
			break
		}
	}
	if first < 0 {
		first = 0
	}
	start, end := max(first-80, 0), min(first+140, len(tr))
	var b strings.Builder
	if start > 0 {
		b.WriteString("… ")
	}
	for i := start; i < end; {
		matched := 0
		for _, t := range rterms {
			if len(t) > matched && hasAt(i, t) {
				matched = len(t)
			}
		}
		if matched > 0 {
			b.WriteString("<mark>" + template.HTMLEscapeString(string(tr[i:i+matched])) + "</mark>")
			i += matched
			continue
		}
		b.WriteString(template.HTMLEscapeString(string(tr[i])))
		i++
	}
	if end < len(tr) {
		b.WriteString(" …")
	}
	return template.HTML(b.String())
}

func (s *Server) handleDocsSearch(w http.ResponseWriter, r *http.Request, user string) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) > 200 {
		q = q[:200]
	}
	s.render(w, r, "docsearch", pageData{Title: "Docs · Search", Nav: "docs", User: user,
		Data: map[string]any{"Page": "", "Pages": docPages, "Q": q, "Hits": s.searchDocs(requestLanguage(r).Code, q)}})
}
