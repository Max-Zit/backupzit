package server

import (
	"html"
	"html/template"
	"regexp"
	"strings"
)

var (
	mdCode = regexp.MustCompile("`([^`]+)`")
	mdBold = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	mdLink = regexp.MustCompile(`\[([^\]]+)\]\((https://[^\s)]+)\)`)
)

// markdownHTML renders the small Markdown subset of release notes
// (headings, lists, paragraphs, **bold**, `code`, [links](https://…)).
// Everything is escaped first, so the notes cannot inject markup.
func markdownHTML(src string) template.HTML {
	var b strings.Builder
	inList := false
	closeList := func() {
		if inList {
			b.WriteString("</ul>")
			inList = false
		}
	}
	inline := func(s string) string {
		s = html.EscapeString(s)
		s = mdCode.ReplaceAllString(s, "<code>$1</code>")
		s = mdBold.ReplaceAllString(s, "<b>$1</b>")
		return mdLink.ReplaceAllString(s, `<a href="$2" rel="noopener" target="_blank">$1</a>`)
	}
	var para []string
	flush := func() {
		if len(para) > 0 {
			b.WriteString("<p>" + inline(strings.Join(para, " ")) + "</p>")
			para = nil
		}
	}
	for _, line := range strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		switch {
		case t == "":
			flush()
			closeList()
		case strings.HasPrefix(t, "#"):
			flush()
			closeList()
			b.WriteString("<h4>" + inline(strings.TrimSpace(strings.TrimLeft(t, "#"))) + "</h4>")
		case strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* "):
			flush()
			if !inList {
				b.WriteString("<ul>")
				inList = true
			}
			b.WriteString("<li>" + inline(t[2:]) + "</li>")
		default:
			if inList { // continuation of the list item
				s := b.String()
				if strings.HasSuffix(s, "</li>") {
					b.Reset()
					b.WriteString(strings.TrimSuffix(s, "</li>") + " " + inline(t) + "</li>")
					continue
				}
			}
			para = append(para, t)
		}
	}
	flush()
	closeList()
	return template.HTML(b.String())
}
