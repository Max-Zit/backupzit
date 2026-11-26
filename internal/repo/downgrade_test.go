package repo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
)

// M-6 (0.55): replacing config of an encrypted repository with an
// unencrypted one on the storage does not make the agent write plaintext.
func TestOpenRefusesDowngradedConfig(t *testing.T) {
	ctx := context.Background()
	enc, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Init(ctx, enc, Password("recovery-key-1")); err != nil {
		t.Fatal(err)
	}
	plain, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Init(ctx, plain); err != nil {
		t.Fatal(err)
	}
	cfg, err := plain.Load(ctx, "config")
	if err != nil {
		t.Fatal(err)
	}
	// The attacker's config.
	_ = enc.Remove(ctx, "config")
	if err := enc.Save(ctx, "config", cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, enc, Password("recovery-key-1")); !errors.Is(err, ErrNotEncrypted) {
		t.Fatalf("downgraded repository opened: %v", err)
	}
	// An unencrypted repository still opens without a password.
	r, err := Open(ctx, plain)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
}

// M-8 (0.55): a planted key file with argon2 parameters of 0 does not panic
// the process; the real key still opens the repository.
func TestPlantedKeyFileDoesNotPanic(t *testing.T) {
	ctx := context.Background()
	be, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Init(ctx, be, Password("recovery-key-1")); err != nil {
		t.Fatal(err)
	}
	bad := []byte(`{"kdf":"argon2id","time":0,"memory_kib":64,"threads":0,"salt":"AAAA","data":"AAAA"}`)
	if err := be.Save(ctx, "keys/"+Hash(bad).String(), bad); err != nil {
		t.Fatal(err)
	}
	r, err := Open(ctx, be, Password("recovery-key-1"))
	if err != nil {
		t.Fatalf("open with the real key: %v", err)
	}
	r.Close()
	if _, err := Open(ctx, be, Password("wrong-key")); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("wrong key: %v", err)
	}
}

// A8: planted key files with other (cheap but allowed) parameters cannot
// push the real key out of the files an open tries, costly or oversized
// ones are skipped, and a wrong key among too many files says so.
func TestPlantedKeyFilesLimits(t *testing.T) {
	ctx := context.Background()
	be, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Init(ctx, be, Password("recovery-key-1")); err != nil {
		t.Fatal(err)
	}
	plant := func(b []byte) {
		if err := be.Save(ctx, "keys/"+Hash(b).String(), b); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < maxKeyFiles+8; i++ {
		plant([]byte(fmt.Sprintf(`{"kdf":"argon2id","time":1,"memory_kib":8,"threads":1,"salt":"AAAA%04d","data":"AAAA"}`, i)))
	}
	// Costly: 1 GiB and 10 passes were accepted before.
	plant([]byte(`{"kdf":"argon2id","time":10,"memory_kib":1048576,"threads":4,"salt":"AAAA","data":"AAAA"}`))
	plant([]byte(`{"kdf":"argon2id","time":3,"memory_kib":65536,"threads":200,"salt":"AAAA","data":"AAAA"}`))
	// Oversized.
	plant([]byte(`{"kdf":"argon2id","time":3,"memory_kib":65536,"threads":4,"salt":"AAAA","data":"` + strings.Repeat("A", 70<<10) + `"}`))
	start := time.Now()
	r, err := Open(ctx, be, Password("recovery-key-1"))
	if err != nil {
		t.Fatalf("open with the real key among planted files: %v", err)
	}
	r.Close()
	if d := time.Since(start); d > 20*time.Second {
		t.Errorf("open took %v", d)
	}
	_, err = Open(ctx, be, Password("wrong-key"))
	if !errors.Is(err, ErrTooManyKeyFiles) || !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("wrong key among too many files: %v", err)
	}
}
