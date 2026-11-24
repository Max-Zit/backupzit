package archiver

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
)

// fakeCloud makes the files named in names look cloud-only for the test.
func fakeCloud(t *testing.T, names ...string) {
	t.Helper()
	oldEntry, oldLive := inCloud, inCloudLive
	t.Cleanup(func() { inCloud, inCloudLive = oldEntry, oldLive })
	is := func(name string) bool {
		for _, n := range names {
			if n == name {
				return true
			}
		}
		return false
	}
	inCloud = func(fi fs.FileInfo, _ *repo.Node) bool { return is(fi.Name()) }
	inCloudLive = func(p string) bool { return is(filepath.Base(p)) }
}

func cloudRepo(t *testing.T) (*repo.Repository, string) {
	t.Helper()
	be, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(context.Background(), be)
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	for name, data := range map[string]string{"a.txt": "local a", "b.docx": strings.Repeat("in the cloud ", 300), "c.txt": "local c"} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return r, src
}

// storedFiles maps the names of the files of snapshot sn to their nodes.
func storedFiles(t *testing.T, r *repo.Repository, sn *repo.Snapshot) map[string]repo.Node {
	t.Helper()
	out := map[string]repo.Node{}
	var walk func(id repo.ID)
	walk = func(id repo.ID) {
		tr, err := r.LoadTree(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range tr.Nodes {
			switch {
			case n.Type == repo.NodeDir && n.Subtree != nil:
				walk(*n.Subtree)
			case n.Type == repo.NodeFile:
				out[n.Name] = n
			}
		}
	}
	walk(sn.Tree)
	return out
}

func TestCloudOnlyFilesAreLeftOutAndCounted(t *testing.T) {
	r, src := cloudRepo(t)
	fakeCloud(t, "b.docx")
	sn, err := Run(context.Background(), r, Options{Paths: []string{src}, Hostname: "h", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	files := storedFiles(t, r, sn)
	if _, ok := files["b.docx"]; ok {
		t.Error("a cloud-only file is in the snapshot")
	}
	if len(files) != 2 {
		t.Errorf("stored files: %v", files)
	}
	st := sn.Stats
	if len(st.Errors) != 0 {
		t.Errorf("errors: %v", st.Errors)
	}
	size := uint64(len(strings.Repeat("in the cloud ", 300)))
	if st.CloudOnlyFiles != 1 || st.CloudOnlyBytes != size || len(st.CloudOnly) != 1 ||
		st.CloudOnly[0] != (repo.CloudOnlyFolder{Path: src, Files: 1, Bytes: size}) {
		t.Errorf("cloud-only stats: %+v", st)
	}
	if st.Files != 2 || st.Bytes != uint64(len("local a")+len("local c")) || st.FilesNew != 2 {
		t.Errorf("file counts include the cloud-only file: %+v", st)
	}
}

// A file that was backed up while it was on the disk keeps its content in
// later backups after it was freed up (unchanged size and time).
func TestCloudOnlyFileKeepsTheParentContent(t *testing.T) {
	r, src := cloudRepo(t)
	opts := Options{Paths: []string{src}, Hostname: "h", Version: "test"}
	sn1, err := Run(context.Background(), r, opts)
	if err != nil {
		t.Fatal(err)
	}
	fakeCloud(t, "b.docx")
	sn2, err := Run(context.Background(), r, opts)
	if err != nil {
		t.Fatal(err)
	}
	b1, b2 := storedFiles(t, r, sn1)["b.docx"], storedFiles(t, r, sn2)["b.docx"]
	if len(b2.Content) == 0 || len(b1.Content) != len(b2.Content) || b1.Content[0] != b2.Content[0] {
		t.Errorf("content of the freed file not kept: %v / %v", b1.Content, b2.Content)
	}
	if sn2.Stats.CloudOnlyFiles != 0 || len(sn2.Stats.Errors) != 0 {
		t.Errorf("stats: %+v", sn2.Stats)
	}
}

// A file whose read fails and that is cloud-only on the live volume (a
// snapshot without the flag) is counted as cloud-only, not as an error.
func TestCloudOnlyLiveFallback(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a file the test cannot read (chmod 000 as a normal user)")
	}
	r, src := cloudRepo(t)
	if err := os.Chmod(filepath.Join(src, "b.docx"), 0); err != nil {
		t.Fatal(err)
	}
	fakeCloud(t)
	inCloudLive = func(p string) bool { return filepath.Base(p) == "b.docx" }
	sn, err := Run(context.Background(), r, Options{Paths: []string{src}, Hostname: "h", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if st := sn.Stats; len(st.Errors) != 0 || st.CloudOnlyFiles != 1 || st.Files != 2 || st.FilesNew != 2 {
		t.Errorf("stats: %+v", st)
	}
}

func TestCloudOnlyFolder(t *testing.T) {
	base := string(filepath.Separator)
	if runtime.GOOS == "windows" {
		base = `C:\`
	}
	p := func(s string) string { return filepath.Join(base, filepath.FromSlash(s)) }
	a := &Archiver{roots: []string{base, p("Users/anna/Documents"), p("data")}}
	for in, want := range map[string]string{
		p("Users/anna/Documents/Projects/x.pdf"): p("Users/anna/Documents"),
		p("Users/bob/Desktop/notes/y.pdf"):       p("Users/bob/Desktop"),
		p("Users/bob/z.pdf"):                     p("Users/bob"),
		p("data/reports/2025/q1.xlsx"):           p("data"),
		p("other/file.txt"):                      base,
	} {
		if got := a.cloudOnlyFolder(in); got != want {
			t.Errorf("cloudOnlyFolder(%s) = %s, want %s", in, got, want)
		}
	}
	// Folders beyond the limit only count in the totals.
	b := &Archiver{roots: []string{base}}
	for i := 0; i < repo.MaxCloudOnlyFolders+3; i++ {
		b.stats.Files++ // archiveFile counts a file before it is skipped
		b.stats.Bytes += 10
		_ = b.skipCloudOnly(p("Users/u"+strings.Repeat("x", i+1)+"/Documents/f.txt"), 10, false, false)
	}
	if len(b.stats.CloudOnly) != repo.MaxCloudOnlyFolders || b.stats.CloudOnlyFiles != repo.MaxCloudOnlyFolders+3 {
		t.Errorf("folders %d, files %d", len(b.stats.CloudOnly), b.stats.CloudOnlyFiles)
	}
}
