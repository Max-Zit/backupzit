package server

import (
	"mime"
	"net/http/httptest"
	"strings"
	"testing"
)

// Names inside a downloaded ZIP stay below the folder it is unpacked to,
// whatever names the backup holds (issue #32 review).
func TestZipEntryName(t *testing.T) {
	for in, want := range map[string]string{
		`\Users\ana\doc.txt`:            "Users/ana/doc.txt",
		"report.txt":                    "report.txt",
		"../../etc/passwd":              "etc/passwd",
		`..\..\Windows\evil.dll`:        "Windows/evil.dll",
		"/abs/./x//y":                   "abs/x/y",
		"a/\x00b\r\n/c":                 "a/b/c",
		"":                              "file",
		"..":                            "file",
		"日本語/ファイル.txt":                  "日本語/ファイル.txt",
		"Büro/Übersicht März 2026.xlsx": "Büro/Übersicht März 2026.xlsx",
	} {
		if got := zipEntryName(in); got != want {
			t.Errorf("zipEntryName(%q) = %q, want %q", in, got, want)
		}
	}
}

// The audit log names what left the backup, bounded in size.
func TestAuditPaths(t *testing.T) {
	if got := auditPaths([]string{`\Users\ana\a.txt`, "b c"}); got != `"\\Users\\ana\\a.txt", "b c"` {
		t.Errorf("two paths: %s", got)
	}
	many := make([]string, 25)
	for i := range many {
		many[i] = strings.Repeat("x", 300)
	}
	got := auditPaths(many)
	if !strings.HasSuffix(got, " +5 more") || strings.Count(got, `"`) != 40 || len(got) > 20*210 {
		t.Errorf("many paths: %d bytes, ends %q", len(got), got[len(got)-12:])
	}
}

// Non-ASCII names arrive intact (RFC 6266), and the browser never sniffs
// the content type of a download.
func TestSetAttachment(t *testing.T) {
	for _, name := range []string{"report.txt", "Übersicht März.xlsx", "請求書 2026-11.pdf", `a"b;c.txt`} {
		w := httptest.NewRecorder()
		setAttachment(w, name, "application/octet-stream", 12)
		_, params, err := mime.ParseMediaType(w.Header().Get("Content-Disposition"))
		if err != nil || params["filename"] != name {
			t.Errorf("%q: %q %v", name, w.Header().Get("Content-Disposition"), err)
		}
		if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Content-Length") != "12" {
			t.Errorf("%q: headers %v", name, w.Header())
		}
	}
}
