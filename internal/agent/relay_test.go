package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/backend"
)

// The agent only reads for the console, only inside the repository (#63).
func TestRelayRead(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "data", "ab"), 0o755)
	os.WriteFile(filepath.Join(dir, "config"), []byte("0123456789"), 0o600)
	os.WriteFile(filepath.Join(dir, "data", "ab", "abcd"), []byte("pack"), 0o600)
	os.WriteFile(filepath.Join(filepath.Dir(dir), "secret.txt"), []byte("outside"), 0o600)
	bes := &relayBackends{m: map[string]*relayBackend{}}
	defer bes.closeAll()
	rs := api.Repository{URL: dir, Password: "never used"}
	read := func(op, name string, off int64, n int) ([]byte, error) {
		return relayRead(ctx, bes, api.RelayRequest{Repository: rs, Op: op, Name: name, Offset: off, Length: n})
	}
	if b, err := read(api.RelayLoad, "config", 0, 0); err != nil || string(b) != "0123456789" {
		t.Errorf("load: %q %v", b, err)
	}
	if b, err := read(api.RelayLoadRange, "config", 2, 3); err != nil || string(b) != "234" {
		t.Errorf("range: %q %v", b, err)
	}
	if b, err := read(api.RelaySize, "config", 0, 0); err != nil || string(b) != "10" {
		t.Errorf("size: %q %v", b, err)
	}
	b, err := read(api.RelayList, "data", 0, 0)
	var names []string
	if err != nil || json.Unmarshal(b, &names) != nil || len(names) != 1 {
		t.Errorf("list: %q %v", b, err)
	}
	if _, err := read(api.RelayLoad, "data/none", 0, 0); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	for _, bad := range []api.RelayRequest{
		{Op: "save", Name: "config"}, {Op: "remove", Name: "config"},
		{Op: api.RelayLoad, Name: "../secret.txt"}, {Op: api.RelayLoad, Name: "/etc/passwd"},
		{Op: api.RelayLoad, Name: `data\..\..\secret.txt`}, {Op: api.RelayLoad, Name: ""},
		{Op: api.RelayLoadRange, Name: "config", Length: 1 << 30},
	} {
		bad.Repository = rs
		if b, err := relayRead(ctx, bes, bad); err == nil {
			t.Errorf("%+v answered %q", bad, b)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "config")); err != nil {
		t.Error("the repository was changed")
	}
}
