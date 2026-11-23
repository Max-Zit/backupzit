package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// Every text a template translates with {{T "..."}} must be in the
// catalogs (the Serbian one is the reference: TestCatalogs checks the
// others against it). Without this a new console section stays English
// in every language.
func TestTemplateTextsAreTranslated(t *testing.T) {
	tCall := regexp.MustCompile(`\{\{-?\s*T\s+("(?:[^"\\]|\\.)*")`)
	files, err := filepath.Glob(filepath.Join("templates", "*.html"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no templates: %v", err)
	}
	sr := languageByCode("sr")
	for _, f := range files {
		b, err := os.ReadFile(f) // #nosec G304 -- test reads its own templates
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range tCall.FindAllStringSubmatch(string(b), -1) {
			k, err := strconv.Unquote(m[1])
			if err != nil {
				continue
			}
			if _, ok := sr.msgs[k]; !ok {
				t.Errorf("%s: %q has no translation", filepath.Base(f), k)
			}
		}
	}
}

// The messages the access package handlers show are translated too.
func TestAccessPackageMessagesAreTranslated(t *testing.T) {
	sr := languageByCode("sr")
	for _, k := range []string{
		"The passphrases do not match.",
		"The passphrase is too short.",
		"Choose at least one backup.",
	} {
		if _, ok := sr.msgs[k]; !ok {
			t.Errorf("%q has no translation", k)
		}
	}
}
