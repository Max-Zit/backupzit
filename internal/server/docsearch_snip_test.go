package server

import (
	"strings"
	"testing"
)

func TestSnippetUnicodeCase(t *testing.T) {
	for _, text := range []string{"İİİİ yedekleme İstanbul", strings.Repeat("İ", 120) + " backup " + strings.Repeat("K", 200)} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%q: panic %v", text[:20], r)
				}
			}()
			s := string(snippet(text, foldCase(text), []string{"backup", "yedekleme"}))
			if !strings.Contains(s, "<mark>") {
				t.Errorf("no mark: %q", s)
			}
		}()
	}
}

func TestSnippetTurkishTerm(t *testing.T) {
	text := "Yedekler İstanbul sunucusunda"
	s := string(snippet(text, foldCase(text), []string{foldCase("İSTANBUL")}))
	if !strings.Contains(s, "<mark>İstanbul</mark>") {
		t.Errorf("snippet %q", s)
	}
}
