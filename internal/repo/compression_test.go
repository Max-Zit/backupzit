package repo_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/repo"
)

// TestCompressionLevels stores the same compressible data at every level:
// reading gives it back, "off" stores it raw, "max" is not larger than the
// default, and repositories mix levels freely.
func TestCompressionLevels(t *testing.T) {
	ctx := context.Background()
	// Real text: the Go sources of this package.
	var text bytes.Buffer
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		b, _ := os.ReadFile(f)
		text.Write(b)
	}
	be, err := backend.OpenLocal(filepath.Join(t.TempDir(), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetCompression("best"); err == nil {
		t.Error("unknown level accepted")
	}
	stored := map[string]uint64{}
	ids := map[string]repo.ID{}
	for i, level := range []string{repo.CompressionOff, repo.CompressionFast, repo.CompressionDefault, repo.CompressionMax} {
		if err := r.SetCompression(level); err != nil {
			t.Fatal(err)
		}
		data := append([]byte(fmt.Sprintf("variant %d\n", i)), text.Bytes()...)
		before := r.Stats().StoredBytes
		id, _, err := r.SaveBlob(ctx, repo.DataBlob, data)
		if err != nil {
			t.Fatal(err)
		}
		stored[level], ids[level] = r.Stats().StoredBytes-before, id
		if err := r.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		got, err := r.LoadBlob(ctx, repo.DataBlob, id)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("%q: read back wrong (%v)", level, err)
		}
	}
	raw := uint64(text.Len())
	if stored["off"] < raw {
		t.Errorf("off: %d bytes stored for %d", stored["off"], raw)
	}
	if stored["fast"] >= raw/2 || stored[""] >= raw/2 {
		t.Errorf("compressible data hardly compressed: fast %d, default %d of %d", stored["fast"], stored[""], raw)
	}
	if stored["max"] > stored[""] {
		t.Errorf("max %d larger than default %d", stored["max"], stored[""])
	}
	t.Logf("of %d bytes: off %d, fast %d, default %d, max %d", raw, stored["off"], stored["fast"], stored[""], stored["max"])
}
