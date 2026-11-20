package imaging

import (
	"bytes"
	"compress/bzip2"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// openLinksStreams opens testdata/links-streams.ntfs.bz2, an NTFS volume
// made by Windows Server 2025 (format, then):
//
//	\a.txt, \b.txt                 one file, two names (mklink /H)
//	\dir1\one.txt, \dir2\two.txt   one file, names in two folders
//	\ads.txt                       "main stream", streams secret and Zone.Identifier
//	\many\file-001..120.txt        the even ones deleted again
//
// ($LogFile is zeroed to keep the file small.)
func openLinksStreams(t *testing.T) *Volume {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "links-streams.ntfs.bz2"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := io.ReadAll(bzip2.NewReader(f))
	if err != nil {
		t.Fatal(err)
	}
	v, err := OpenNTFS(bytes.NewReader(img))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func names(t *testing.T, v *Volume, dir string) []string {
	t.Helper()
	es, err := v.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name)
	}
	return out
}

func readAll(t *testing.T, v *Volume, p string) string {
	t.Helper()
	r, _, err := v.ReadFile(p)
	if err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	return string(b)
}

// TestNTFSHardLinks: every name of a file with hard links is listed and
// readable in its own folder (go-ntfs ListDir lists a file once, under one
// of its names, possibly from another folder).
func TestNTFSHardLinks(t *testing.T) {
	v := openLinksStreams(t)
	root := names(t, v, `\`)
	for _, want := range []string{"a.txt", "b.txt", "dir1", "dir2"} {
		if !slices.Contains(root, want) {
			t.Errorf(`\ lists %q, missing %q`, root, want)
		}
	}
	if got := names(t, v, `\dir1`); !slices.Equal(got, []string{"one.txt"}) {
		t.Errorf(`\dir1 lists %q, want [one.txt]`, got)
	}
	if got := names(t, v, `\dir2`); !slices.Equal(got, []string{"two.txt"}) {
		t.Errorf(`\dir2 lists %q, want [two.txt]`, got)
	}
	for _, p := range []string{`\a.txt`, `\b.txt`} {
		if got := readAll(t, v, p); got != "two names, one file" {
			t.Errorf("%s = %q", p, got)
		}
	}
	for _, p := range []string{`\dir1\one.txt`, `\dir2\two.txt`} {
		if got := readAll(t, v, p); got != "linked across folders" {
			t.Errorf("%s = %q", p, got)
		}
	}
}

// TestNTFSDeletedIndexEntries: deleted files do not come back from index
// blocks or slack.
func TestNTFSDeletedIndexEntries(t *testing.T) {
	v := openLinksStreams(t)
	got := names(t, v, `\many`)
	if len(got) != 60 {
		t.Errorf(`\many lists %d files, want 60`, len(got))
	}
	for _, n := range got {
		var i int
		if _, err := fmt.Sscanf(n, "file-%d.txt", &i); err != nil || i%2 == 0 {
			t.Errorf("unexpected entry %q", n)
		}
	}
}

// TestNTFSStreams: alternate data streams are listed as "name:stream" and
// read with their own data, not the file's main data.
func TestNTFSStreams(t *testing.T) {
	v := openLinksStreams(t)
	root := names(t, v, `\`)
	for _, want := range []string{"ads.txt", "ads.txt:secret", "ads.txt:Zone.Identifier"} {
		if !slices.Contains(root, want) {
			t.Errorf(`\ lists %q, missing %q`, root, want)
		}
	}
	if got := readAll(t, v, `\ads.txt`); got != "main stream" {
		t.Errorf(`\ads.txt = %q`, got)
	}
	if got := readAll(t, v, `\ads.txt:secret`); got != "hidden stream" {
		t.Errorf(`\ads.txt:secret = %q, want "hidden stream"`, got)
	}
	if got := readAll(t, v, `\ads.txt:Zone.Identifier`); !strings.Contains(got, "ZoneId=3") {
		t.Errorf(`\ads.txt:Zone.Identifier = %q`, got)
	}
	e, err := v.Stat(`\ads.txt:secret`)
	if err != nil || e.Size != int64(len("hidden stream")) {
		t.Errorf("Stat = %+v, %v", e, err)
	}
}

// TestNTFSExtractLinksAndStreams restores the whole volume: both names of
// the hard links, and the streams (on Windows into the restored file's
// stream, elsewhere as a file named "name:stream").
func TestNTFSExtractLinksAndStreams(t *testing.T) {
	v := openLinksStreams(t)
	dir := t.TempDir()
	st, err := v.Extract(context.Background(), []string{`\`}, func(p string) string {
		return filepath.Join(dir, filepath.FromSlash(strings.ReplaceAll(p, `\`, "/")))
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Errors) > 0 {
		t.Fatalf("errors: %q", st.Errors)
	}
	for p, want := range map[string]string{
		"a.txt":          "two names, one file",
		"b.txt":          "two names, one file",
		"dir1/one.txt":   "linked across folders",
		"dir2/two.txt":   "linked across folders",
		"ads.txt":        "main stream",
		"ads.txt:secret": "hidden stream",
	} {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		if string(b) != want {
			t.Errorf("%s = %q, want %q", p, b, want)
		}
	}
}
