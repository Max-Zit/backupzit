package server

import (
	"strings"
	"testing"
)

func TestMarkdownHTML(t *testing.T) {
	got := string(markdownHTML("## New\n\n- **All disks** — `image-backup --all-disks`\n  continued\n- see [docs](https://example.com/a)\n\nPlain <script>alert(1)</script> text."))
	for _, want := range []string{"<h4>New</h4>", "<ul><li><b>All disks</b> — <code>image-backup --all-disks</code> continued</li>",
		`<a href="https://example.com/a" rel="noopener" target="_blank">docs</a>`, "&lt;script&gt;"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	if strings.Contains(got, "<script>") || strings.Contains(string(markdownHTML("[x](javascript:alert(1))")), "href") {
		t.Error("unsafe markup")
	}
}
