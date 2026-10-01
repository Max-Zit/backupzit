package repo

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/backupzit/backupzit/internal/backend"
	"golang.org/x/crypto/argon2"
)

// Encryption: every blob, pack header, index file and snapshot file is
// sealed with AES-256-GCM under a random master key (12 byte random nonce
// prefixed, 16 byte tag appended), which also authenticates the data.
// The master key and the chunker polynomial live in keys/<id>, sealed with
// a key derived from the repository password with Argon2id. The plaintext
// config only says that the repository is encrypted.

// EncryptionAES256GCM is the only supported scheme.
const EncryptionAES256GCM = "aes256-gcm"

// ErrPasswordRequired is returned when opening an encrypted repository
// without a password.
var ErrPasswordRequired = errors.New("repository is encrypted: a password (recovery key) is required")

// ErrWrongPassword means no key file could be opened with the password.
var ErrWrongPassword = errors.New("wrong repository password (recovery key)")

// Option configures Init and Open.
type Option func(*options)

type options struct{ password string }

// Password sets the repository password. With Init it creates an encrypted
// repository; with Open it unlocks one.
func Password(pw string) Option { return func(o *options) { o.password = pw } }

type keyFile struct {
	KDF     string    `json:"kdf"`
	Time    uint32    `json:"time"`
	Memory  uint32    `json:"memory_kib"`
	Threads uint8     `json:"threads"`
	Salt    []byte    `json:"salt"`
	Data    []byte    `json:"data"` // sealed masterKey JSON
	Created time.Time `json:"created"`
}

type masterKey struct {
	Key               []byte `json:"key"`
	ChunkerPolynomial string `json:"chunker_polynomial"`
}

// Argon2id parameters (RFC 9106 second recommended option).
const (
	kdfTime    = 3
	kdfMemory  = 64 * 1024
	kdfThreads = 4
)

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func sealWith(a cipher.AEAD, plain []byte) []byte {
	nonce := make([]byte, a.NonceSize(), a.NonceSize()+len(plain)+a.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	return a.Seal(nonce, nonce, plain, nil)
}

func openWith(a cipher.AEAD, c []byte) ([]byte, error) {
	if len(c) < a.NonceSize()+a.Overhead() {
		return nil, errors.New("ciphertext too short")
	}
	return a.Open(nil, c[:a.NonceSize()], c[a.NonceSize():], nil)
}

// seal encrypts p if the repository is encrypted.
func (r *Repository) seal(p []byte) []byte {
	if r.aead == nil {
		return p
	}
	return sealWith(r.aead, p)
}

// open decrypts and authenticates c if the repository is encrypted.
func (r *Repository) open(c []byte) ([]byte, error) {
	if r.aead == nil {
		return c, nil
	}
	p, err := openWith(r.aead, c)
	if err != nil {
		return nil, errors.New("decryption failed (data corrupted or modified)")
	}
	return p, nil
}

// Encrypted reports whether the repository is encrypted.
func (r *Repository) Encrypted() bool { return r.aead != nil }

// createKey generates a master key, stores it sealed under password and
// returns it.
func createKey(ctx context.Context, be backend.Backend, password string, pol string) (*masterKey, error) {
	mk := &masterKey{Key: make([]byte, 32), ChunkerPolynomial: pol}
	if _, err := rand.Read(mk.Key); err != nil {
		return nil, err
	}
	if err := addKey(ctx, be, mk, password); err != nil {
		return nil, err
	}
	return mk, nil
}

// addKey stores another key file that unlocks mk with password.
func addKey(ctx context.Context, be backend.Backend, mk *masterKey, password string) error {
	kf := keyFile{KDF: "argon2id", Time: kdfTime, Memory: kdfMemory, Threads: kdfThreads,
		Salt: make([]byte, 32), Created: time.Now().UTC()}
	if _, err := rand.Read(kf.Salt); err != nil {
		return err
	}
	kek, err := newAEAD(argon2.IDKey([]byte(password), kf.Salt, kf.Time, kf.Memory, kf.Threads, 32))
	if err != nil {
		return err
	}
	plain, err := json.Marshal(mk)
	if err != nil {
		return err
	}
	kf.Data = sealWith(kek, plain)
	b, err := json.MarshalIndent(kf, "", "  ")
	if err != nil {
		return err
	}
	return be.Save(ctx, "keys/"+Hash(b).String(), b)
}

// unlock tries every key file with password.
func unlock(ctx context.Context, be backend.Backend, password string) (*masterKey, error) {
	if password == "" {
		return nil, ErrPasswordRequired
	}
	names, err := be.List(ctx, "keys")
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, errors.New("encrypted repository has no key files")
	}
	for _, n := range names {
		b, err := be.Load(ctx, n)
		if err != nil {
			continue
		}
		var kf keyFile
		if json.Unmarshal(b, &kf) != nil || kf.KDF != "argon2id" || kf.Memory > 4<<20 || kf.Time > 100 {
			continue
		}
		kek, err := newAEAD(argon2.IDKey([]byte(password), kf.Salt, kf.Time, kf.Memory, kf.Threads, 32))
		if err != nil {
			continue
		}
		plain, err := openWith(kek, kf.Data)
		if err != nil {
			continue
		}
		var mk masterKey
		if err := json.Unmarshal(plain, &mk); err != nil || len(mk.Key) != 32 {
			return nil, fmt.Errorf("key file %s is damaged", n)
		}
		return &mk, nil
	}
	return nil, ErrWrongPassword
}
