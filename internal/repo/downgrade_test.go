package repo

import (
	"context"
	"errors"
	"testing"

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
