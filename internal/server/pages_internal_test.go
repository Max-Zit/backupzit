package server

import (
	"html/template"
	"io"
	"log/slog"
	"runtime"
	"testing"
)

// Pages are parsed on first use; every page must still parse in every
// language (the console no longer parses them all at start).
func TestAllPagesParse(t *testing.T) {
	s, err := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, l := range languages {
		for name := range s.pageSrc[l.Code] {
			if _, err := s.parsePage(l.Code, name); err != nil {
				t.Errorf("%s/%s: %v", l.Code, name, err)
			}
			n++
		}
		s.pagesMu.Lock()
		s.pages[l.Code] = map[string]*template.Template{} // keep the test's memory low
		s.pagesMu.Unlock()
	}
	if n < 14*50 {
		t.Errorf("only %d pages", n)
	}
}

// A console that has shown nothing yet holds only the login page.
func TestNewServerIsSmall(t *testing.T) {
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	s, err := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	runtime.ReadMemStats(&b)
	if grown := int64(b.HeapAlloc) - int64(a.HeapAlloc); grown > 64<<20 {
		t.Errorf("a new console takes %d MB", grown>>20)
	}
	runtime.KeepAlive(s)
}
