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
					text: text, lower: strings.ToLower(text), lowerHead: strings.ToLower(heading + " " + p.Title)})
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
	for _, w := range strings.Fields(strings.ToLower(q)) {
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

// snippet cuts about 220 characters around the first match and marks the
// words; everything else is escaped.
func snippet(text, lower string, terms []string) template.HTML {
	first := len(lower)
	for _, t := range terms {
		if i := strings.Index(lower, t); i >= 0 && i < first {
			first = i
		}
	}
	if first == len(lower) {
		first = 0
	}
	start, end := first-80, first+140
	if start < 0 {
		start = 0
	}
	if end > len(text) {
		end = len(text)
	}
	for start > 0 && !utf8.RuneStart(text[start]) {
		start--
	}
	for end < len(text) && !utf8.RuneStart(text[end]) {
		end++
	}
	part, lpart := text[start:end], lower[start:end]
	var b strings.Builder
	if start > 0 {
		b.WriteString("… ")
	}
	for i := 0; i < len(part); {
		matched := 0
		for _, t := range terms {
			if strings.HasPrefix(lpart[i:], t) && len(t) > matched {
				matched = len(t)
			}
		}
		if matched > 0 {
			b.WriteString("<mark>" + template.HTMLEscapeString(part[i:i+matched]) + "</mark>")
			i += matched
			continue
		}
		_, n := utf8.DecodeRuneInString(part[i:])
		b.WriteString(template.HTMLEscapeString(part[i : i+n]))
		i += n
	}
	if end < len(text) {
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
