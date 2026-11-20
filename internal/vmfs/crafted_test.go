package vmfs

import (
	"bytes"
	"compress/bzip2"
	"context"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/max-zit/backupzit/internal/imaging"
)

// M-4/M-5 (0.55): names and structures from a guest's disks are untrusted.
func TestSafeNameAndWithin(t *testing.T) {
	for _, n := range []string{"", ".", "..", `..\..\Windows`, "a/b", `a\b`, "x\x00y"} {
		if SafeName(n) {
			t.Errorf("SafeName(%q) = true", n)
		}
	}
	for _, n := range []string{"etc", "notes..txt", ".bashrc", "Ünïcödé"} {
		if !SafeName(n) {
			t.Errorf("SafeName(%q) = false", n)
		}
	}
	base := filepath.Join(t.TempDir(), "restore")
	if !Within(base, filepath.Join(base, "etc", "x")) || !Within(base, base) {
		t.Error("a path inside the folder is refused")
	}
	if Within(base, filepath.Join(base, "..", "outside")) || Within(base, filepath.Dir(base)) {
		t.Error("a path outside the folder is accepted")
	}
}

func TestLVMNestingLimit(t *testing.T) {
	deep := strings.Repeat("a {\n", 100) + strings.Repeat("}\n", 100)
	if _, err := parseLVMConfig(deep); err == nil || !strings.Contains(err.Error(), "too deep") {
		t.Fatalf("deeply nested metadata: %v", err)
	}
	if _, err := parseLVMConfig("vg {\n id = \"x\"\n lv {\n seg {\n start = 0\n }\n }\n}\n"); err != nil {
		t.Fatalf("normal metadata: %v", err)
	}
	// Arrays nest through value(), not section(): without the same cap a
	// few MiB of "[" overflow the stack and kill the process.
	arr := "a = " + strings.Repeat("[", 3<<20)
	if _, err := parseLVMConfig(arr); err == nil || !strings.Contains(err.Error(), "too deep") {
		t.Fatalf("deeply nested array: %v", err)
	}
	// Normal arrays, also nested ones inside sections, still parse.
	cfg, err := parseLVMConfig("vg {\n seg {\n stripes = [\n\"pv0\", 0\n]\n x = [[1], [2]]\n }\n}\n")
	if err != nil {
		t.Fatalf("normal arrays: %v", err)
	}
	seg, _ := cfg["vg"].(map[string]any)["seg"].(map[string]any)
	if s, _ := seg["stripes"].([]any); len(s) != 2 {
		t.Fatalf("stripes parsed wrong: %v", seg)
	}
	if x, _ := seg["x"].([]any); len(x) != 2 {
		t.Fatalf("nested array parsed wrong: %v", seg)
	}
}

func TestGPTEntrySizeBound(t *testing.T) {
	head := make([]byte, 1024)
	copy(head[512:], "EFI PART")
	binary.LittleEndian.PutUint64(head[512+72:], 2)
	binary.LittleEndian.PutUint32(head[512+80:], 1024)
	binary.LittleEndian.PutUint32(head[512+84:], 1<<30) // a crafted entry size of 1 GiB
	if _, err := gptPartitions(bytes.NewReader(make([]byte, 4096)), head); err == nil {
		t.Fatal("a crafted GPT entry size was accepted")
	}
}

// L-N15 (0.56): on Windows a ':' in an ext4/XFS name would pick a stream of
// another file ("a:b" is stream b of a, "a::$DATA" is a itself), so such
// names are skipped and reported there; NTFS streams are still written as
// streams. On Linux ':' is an ordinary character and every name is restored.
func TestExtractColonNames(t *testing.T) {
	ext := &ioFS{fsys: fstest.MapFS{
		"a":        {Data: []byte("sibling")},
		"a:b":      {Data: []byte("a file named a:b")},
		"a::$DATA": {Data: []byte("overwritten")},
		"d:x/f":    {Data: []byte("in a folder named d:x")},
	}}
	dir := t.TempDir()
	read := func(dir, p string) string {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		if err != nil {
			return "<" + err.Error() + ">"
		}
		return string(b)
	}
	st, err := Extract(context.Background(), ext, []string{"/"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(dir, "a"); got != "sibling" {
		t.Errorf("a = %q", got)
	}
	if runtime.GOOS == "windows" {
		if len(st.Errors) != 3 {
			t.Errorf("errors %q", st.Errors)
		}
		for _, e := range st.Errors {
			if !strings.Contains(e, "Windows does not allow ':'") {
				t.Errorf("error %q", e)
			}
		}
		if _, err := os.Stat(filepath.Join(dir, "a:b")); err == nil {
			t.Error("a:b became a stream of a")
		}
	} else {
		if len(st.Errors) > 0 {
			t.Errorf("errors %q", st.Errors)
		}
		for p, want := range map[string]string{"a:b": "a file named a:b", "a::$DATA": "overwritten", "d:x/f": "in a folder named d:x"} {
			if got := read(dir, p); got != want {
				t.Errorf("%s = %q, want %q", p, got, want)
			}
		}
	}

	// NTFS streams keep their name apart from the file name through vmfs.
	f, err := os.Open(filepath.Join("..", "imaging", "testdata", "links-streams.ntfs.bz2"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := io.ReadAll(bzip2.NewReader(f))
	if err != nil {
		t.Fatal(err)
	}
	v, err := imaging.OpenNTFS(bytes.NewReader(img))
	if err != nil {
		t.Fatal(err)
	}
	nd := t.TempDir()
	st, err = Extract(context.Background(), safeFS{ntfsFS{v}}, []string{"/ads.txt", "/ads.txt:secret"}, nd)
	if err != nil || len(st.Errors) > 0 {
		t.Fatalf("NTFS: %v %q", err, st.Errors)
	}
	if got := read(nd, "ads.txt"); got != "main stream" {
		t.Errorf("ads.txt = %q", got)
	}
	if got := read(nd, "ads.txt:secret"); got != "hidden stream" {
		t.Errorf("ads.txt:secret = %q", got)
	}
}
