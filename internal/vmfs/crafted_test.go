package vmfs

import (
	"bytes"
	"encoding/binary"
	"path/filepath"
	"strings"
	"testing"
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
