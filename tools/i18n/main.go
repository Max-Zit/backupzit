// Command i18n helps translating the console.
//
//	go run ./tools/i18n wrap FILE...    mark the visible English text of console templates with {{T "..."}}
//	go run ./tools/i18n keys            list the texts the templates translate (one per line, JSON strings)
//	go run ./tools/i18n missing LANG    texts without a translation in internal/server/i18n/LANG.json
//
// The English text is the key; a catalog maps it to the translation.
package main

import (
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		fail("usage: i18n wrap FILE... | keys | missing LANG")
	}
	switch os.Args[1] {
	case "wrap":
		for _, f := range os.Args[2:] {
			b, err := os.ReadFile(f)
			if err != nil {
				fail("%v", err)
			}
			out := wrap(string(b))
			if out != string(b) {
				if err := os.WriteFile(f, []byte(out), 0o644); err != nil {
					fail("%v", err)
				}
			}
		}
	case "keys":
		for _, k := range keys() {
			fmt.Println(strconv.Quote(k))
		}
	case "missing":
		if len(os.Args) < 3 {
			fail("usage: i18n missing LANG")
		}
		b, err := os.ReadFile(filepath.Join("internal", "server", "i18n", os.Args[2]+".json"))
		if err != nil {
			fail("%v", err)
		}
		var cat map[string]string
		if err := json.Unmarshal(b, &cat); err != nil {
			fail("%v", err)
		}
		n := 0
		for _, k := range keys() {
			if cat[k] == "" {
				fmt.Println(strconv.Quote(k))
				n++
			}
		}
		fmt.Fprintf(os.Stderr, "%d missing\n", n)
	default:
		fail("unknown command %q", os.Args[1])
	}
}

var tCall = regexp.MustCompile(`\{\{-?\s*T\s+("(?:[^"\\]|\\.)*")`)

// keys lists the texts marked in the console templates.
func keys() []string {
	files, _ := filepath.Glob(filepath.Join("internal", "server", "templates", "*.html"))
	seen := map[string]bool{}
	for _, f := range files {
		b, _ := os.ReadFile(f)
		for _, m := range tCall.FindAllStringSubmatch(string(b), -1) {
			if s, err := strconv.Unquote(m[1]); err == nil {
				seen[s] = true
			}
		}
	}
	var out []string
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var letter = regexp.MustCompile(`[A-Za-z]{2,}`)

// translatable attributes
var attrRe = regexp.MustCompile(`\b(placeholder|title|data-confirm|aria-label|alt)="([^"{}]*)"`)

// wrap marks visible text of an HTML template. Template actions, tags,
// and the contents of script, style, pre and code elements are kept.
func wrap(s string) string {
	var b strings.Builder
	skip := "" // element whose content is kept as is
	i := 0
	for i < len(s) {
		switch {
		case strings.HasPrefix(s[i:], "{{"):
			j := strings.Index(s[i:], "}}")
			if j < 0 {
				b.WriteString(s[i:])
				return b.String()
			}
			b.WriteString(s[i : i+j+2])
			i += j + 2
		case s[i] == '<':
			j := tagEnd(s, i)
			tag := s[i:j]
			name := tagName(tag)
			if skip == "" {
				if strings.HasPrefix(tag, "</") {
					// closing tag: nothing to translate
				} else {
					tag = attrRe.ReplaceAllStringFunc(tag, func(a string) string {
						m := attrRe.FindStringSubmatch(a)
						if !letter.MatchString(m[2]) {
							return a
						}
						return m[1] + `="{{T ` + strconv.Quote(html.UnescapeString(m[2])) + `}}"`
					})
					switch name {
					case "script", "style", "pre", "code", "textarea":
						if !strings.HasSuffix(tag, "/>") {
							skip = name
						}
					}
				}
			} else if strings.HasPrefix(tag, "</") && name == skip {
				skip = ""
			}
			b.WriteString(tag)
			i = j
		default:
			j := i
			for j < len(s) && s[j] != '<' && !strings.HasPrefix(s[j:], "{{") {
				j++
			}
			text := s[i:j]
			if skip != "" || !letter.MatchString(text) {
				b.WriteString(text)
			} else {
				lead := len(text) - len(strings.TrimLeft(text, " \t\r\n"))
				core := strings.TrimSpace(text)
				trail := text[lead+len(core):]
				b.WriteString(text[:lead])
				b.WriteString(`{{T ` + strconv.Quote(html.UnescapeString(core)) + `}}`)
				b.WriteString(trail)
			}
			i = j
		}
	}
	return b.String()
}

// tagEnd finds the end of the tag starting at i, skipping template actions
// and quoted attribute values.
func tagEnd(s string, i int) int {
	q := byte(0)
	for j := i + 1; j < len(s); j++ {
		switch {
		case strings.HasPrefix(s[j:], "{{"):
			k := strings.Index(s[j:], "}}")
			if k < 0 {
				return len(s)
			}
			j += k + 1
		case q != 0:
			if s[j] == q {
				q = 0
			}
		case s[j] == '"' || s[j] == '\'':
			q = s[j]
		case s[j] == '>':
			return j + 1
		}
	}
	return len(s)
}

func tagName(tag string) string {
	t := strings.TrimLeft(tag, "</")
	for k, c := range t {
		if c == ' ' || c == '>' || c == '/' || c == '\n' || c == '\t' {
			return strings.ToLower(t[:k])
		}
	}
	return strings.ToLower(t)
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "i18n: "+format+"\n", a...)
	os.Exit(1)
}
