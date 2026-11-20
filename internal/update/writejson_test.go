package update

import (
	"os"
	"path/filepath"
	"testing"
)

// M-10 (0.55): a symlink planted at the old fixed temporary name does not
// make the root update helper overwrite the file it points to.
func TestWriteJSONIgnoresPlantedSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "result.json")
	if err := os.Symlink(victim, p+".tmp"); err != nil {
		t.Skip("symlinks not available:", err)
	}
	if err := WriteJSON(p, map[string]string{"state": "done"}, 0o644); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Fatalf("the symlink target was overwritten: %q", b)
	}
	var got map[string]string
	if err := ReadJSON(p, &got); err != nil || got["state"] != "done" {
		t.Fatalf("result: %v %v", got, err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".result.json.*.tmp")); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
}
