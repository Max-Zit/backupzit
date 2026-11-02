// Package uicheck reads the console's pages the way a browser does and
// checks what a click would do, not only that a page loads. An HTML5 parser
// builds the same tree as the browser, so a stray </form> that leaves a
// button outside its form (it then does nothing — issue #9) shows here
// although a test posting the form directly passes.
//
// Used by the server tests (TestUIWalk) and by tools/uiwalk against a real
// console before a release.
package uicheck

import (
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"

	"golang.org/x/net/html"
)

// Client is a signed-in browser session.
type Client interface {
	// Do sends a request (form nil for GET) and returns the status, the
	// Location header of a redirect and the body.
	Do(method, path string, form url.Values) (int, string, string)
}

// Result of a walk.
type Result struct {
	Pages    int      // pages read
	Edits    []string // edit pages found
	Saved    []string // edit pages sent back unchanged
	Problems []string
}

func (r *Result) add(format string, a ...any) {
	r.Problems = append(r.Problems, fmt.Sprintf(format, a...))
}

// Options of a walk.
type Options struct {
	MaxPages int // default 800
	// RoundTrip sends every edit page back unchanged and expects the same
	// values afterwards. Pages whose form has a field named in Skip are
	// left out (e.g. m365_tenant: saving checks the app at Microsoft).
	RoundTrip bool
	Skip      []string
}

// Walk reads every page reachable from "/" and checks it.
func Walk(c Client, o Options) Result {
	if o.MaxPages == 0 {
		o.MaxPages = 800
	}
	var r Result
	seen := map[string]bool{"/": true}
	queue := []string{"/"}
	for len(queue) > 0 && r.Pages < o.MaxPages {
		p := queue[0]
		queue = queue[1:]
		code, _, body := c.Do("GET", p, nil)
		if code != 200 || !strings.Contains(body, "<html") {
			continue
		}
		r.Pages++
		doc, err := html.Parse(strings.NewReader(body))
		if err != nil {
			r.add("%s: %v", p, err)
			continue
		}
		CheckPage(&r, p, doc, body)
		if strings.HasSuffix(p, "/edit") {
			r.Edits = append(r.Edits, p)
		}
		for _, href := range attrs(doc, "a", "href") {
			if n := walkable(p, href); n != "" && !seen[n] {
				seen[n] = true
				queue = append(queue, n)
			}
		}
	}
	sort.Strings(r.Edits)
	if o.RoundTrip {
		for _, p := range r.Edits {
			if roundTrip(&r, c, p, o.Skip) {
				r.Saved = append(r.Saved, p)
			}
		}
	}
	return r
}

// walkable resolves an internal link worth reading, or returns "".
func walkable(from, href string) string {
	u, err := url.Parse(href)
	if err != nil || u.Scheme != "" || u.Host != "" || u.Path == "" {
		return ""
	}
	if !strings.HasPrefix(u.Path, "/") {
		u.Path = path.Join(path.Dir(from), u.Path)
	}
	for _, skip := range []string{"/static/", "/logout", "/download", "/export", ".csv", ".pdf", ".json", "/api/", "/dist/"} {
		if strings.Contains(u.Path, skip) {
			return ""
		}
	}
	// Pages differ by their path; queries are mostly filters and messages.
	return u.Path
}

func attr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

func attrs(doc *html.Node, tag, key string) []string {
	var out []string
	walk(doc, func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == tag {
			if v, ok := attr(n, key); ok {
				out = append(out, v)
			}
		}
	})
	return out
}

func walk(n *html.Node, f func(*html.Node)) {
	f(n)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(c, f)
	}
}

func inside(n *html.Node, tag string) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode && p.Data == tag {
			return true
		}
	}
	return false
}

func isSubmit(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	typ, _ := attr(n, "type")
	switch n.Data {
	case "button":
		return typ == "" || typ == "submit"
	case "input":
		return typ == "submit" || typ == "image"
	}
	return false
}

func text(n *html.Node) string {
	var b strings.Builder
	walk(n, func(c *html.Node) {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		}
	})
	return b.String()
}

// CheckPage checks one parsed page p: submit buttons outside every form,
// duplicate ids and template leftovers.
func CheckPage(r *Result, p string, doc *html.Node, body string) {
	ids := map[string]int{}
	forms := map[string]bool{}
	walk(doc, func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		if id, ok := attr(n, "id"); ok {
			ids[id]++
			if n.Data == "form" {
				forms[id] = true
			}
		}
	})
	walk(doc, func(n *html.Node) {
		if !isSubmit(n) || inside(n, "form") {
			return
		}
		if f, ok := attr(n, "form"); ok && forms[f] {
			return
		}
		r.add("%s: button %q is outside every form, a click does nothing", p, strings.TrimSpace(text(n)))
	})
	var dup []string
	for id, n := range ids {
		if n > 1 {
			dup = append(dup, fmt.Sprintf("%q ×%d", id, n))
		}
	}
	if len(dup) > 0 {
		sort.Strings(dup)
		r.add("%s: duplicate ids %s", p, strings.Join(dup, ", "))
	}
	for _, bad := range []string{"{{", "<no value>", "%!", "MISSING"} {
		if i := strings.Index(body, bad); i >= 0 {
			r.add("%s: template leftover %q near %q", p, bad, body[max(0, i-60):min(len(body), i+40)])
		}
	}
}

// FormValues returns what a browser sends for the form posting to action:
// the enabled controls that are not in a hidden part (the console's script
// disables those), checked boxes and radios, selected options.
func FormValues(doc *html.Node, action string) (url.Values, bool) {
	var form *html.Node
	walk(doc, func(n *html.Node) {
		if form == nil && n.Type == html.ElementNode && n.Data == "form" {
			if a, _ := attr(n, "action"); a == action {
				form = n
			}
		}
	})
	if form == nil {
		return nil, false
	}
	v := url.Values{}
	walk(form, func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		name, ok := attr(n, "name")
		if !ok || name == "" {
			return
		}
		if _, dis := attr(n, "disabled"); dis {
			return
		}
		for p := n; p != nil && p != form; p = p.Parent {
			if _, h := attr(p, "hidden"); h {
				return
			}
		}
		val, hasVal := attr(n, "value")
		switch n.Data {
		case "input":
			typ, _ := attr(n, "type")
			switch typ {
			case "checkbox", "radio":
				if _, on := attr(n, "checked"); !on {
					return
				}
				if !hasVal {
					val = "on"
				}
			case "submit", "button", "image", "reset", "file":
				return
			}
			v.Add(name, val)
		case "textarea":
			v.Add(name, text(n))
		case "select":
			_, multi := attr(n, "multiple")
			var opts, sel []string
			walk(n, func(o *html.Node) {
				if o.Type != html.ElementNode || o.Data != "option" {
					return
				}
				if _, d := attr(o, "disabled"); d {
					return
				}
				ov, ok := attr(o, "value")
				if !ok {
					ov = strings.TrimSpace(text(o))
				}
				opts = append(opts, ov)
				if _, s := attr(o, "selected"); s {
					sel = append(sel, ov)
				}
			})
			if len(sel) == 0 && !multi && len(opts) > 0 {
				sel = opts[:1]
			}
			if !multi && len(sel) > 1 {
				sel = sel[len(sel)-1:]
			}
			for _, s := range sel {
				v.Add(name, s)
			}
		}
	})
	return v, true
}

// roundTrip saves edit page p without changes and expects the same values;
// it reports whether the page was sent.
func roundTrip(r *Result, c Client, p string, skip []string) bool {
	read := func() url.Values {
		_, _, body := c.Do("GET", p, nil)
		doc, err := html.Parse(strings.NewReader(body))
		if err != nil {
			return nil
		}
		v, ok := FormValues(doc, p)
		if !ok {
			r.add("%s: no form posting to the page", p)
		}
		return v
	}
	before := read()
	if before == nil {
		return false
	}
	for _, s := range skip {
		if before.Has(s) {
			return false
		}
	}
	_, loc, _ := c.Do("POST", p, before)
	if !strings.Contains(loc, "msg=") || strings.Contains(loc, "err=") {
		msg, _ := url.QueryUnescape(loc)
		r.add("%s: saving unchanged failed: %s", p, msg)
		return true
	}
	if b, a := before.Encode(), read().Encode(); b != a {
		r.add("%s: saving without changes changed the form\nbefore %s\nafter  %s", p, b, a)
	}
	return true
}
