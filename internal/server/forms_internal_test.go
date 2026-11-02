package server

import (
	"io/fs"
	"regexp"
	"testing"
)

// Every page template closes each form once: a stray </form> ends the form
// early, and the browser then ignores the buttons after it (issue #9, the
// "Save changes" button of the job edit page did nothing).
func TestTemplateFormsBalanced(t *testing.T) {
	open, closing := regexp.MustCompile(`<form[\s>]`), regexp.MustCompile(`</form>`)
	err := fs.WalkDir(templateFS, "templates", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := templateFS.ReadFile(p)
		if err != nil {
			return err
		}
		if o, c := len(open.FindAll(b, -1)), len(closing.FindAll(b, -1)); o != c {
			t.Errorf("%s: %d <form> but %d </form>", p, o, c)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
