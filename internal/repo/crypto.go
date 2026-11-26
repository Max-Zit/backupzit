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

	"github.com/max-zit/backupzit/internal/backend"
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

// ErrNotEncrypted is returned when a repository opened with a password
// (recovery key) says it is not encrypted: its config may have been
// replaced on the storage, and nothing is written to it in plaintext.
var ErrNotEncrypted = errors.New("the repository is not encrypted although a recovery key was given (its config may have been changed on the storage); nothing is written to it")

// ErrPackDamaged marks a pack that is proven damaged on the storage: it is
// missing, or a read of all its bytes does not match its content hash.
// Errors without it (a download cut short, a timeout) say nothing about
// the pack itself: it may only be the way to this machine (#38).
var ErrPackDamaged = errors.New("damaged on the storage")

// ErrWrongPassword means no key file could be opened with the password.
var ErrWrongPassword = errors.New("wrong repository password (recovery key)")

// Option configures Init and Open.
type Option func(*options)

type options struct {
	password      string
	indexProgress func(done, total int)
}

// Password sets the repository password. With Init it creates an encrypted
// repository; with Open it unlocks one.
func Password(pw string) Option { return func(o *options) { o.password = pw } }

// IndexProgress is called with Open as the index files are read (done of
// total), from several goroutines on object stores; it must be quick.
func IndexProgress(f func(done, total int)) Option { return func(o *options) { o.indexProgress = f } }

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
	// maxKeyFiles is how many valid key files an open tries, maxKeyListing
	// how many it reads at all.
	maxKeyFiles   = 32
	maxKeyListing = 1000
	// Limits for key files found in the storage: anyone who can write
	// there can plant one, and a costly key derivation per open would
	// exhaust the agent's or the console's memory and time. BackupZit only
	// ever wrote kdfTime/kdfMemory/kdfThreads.
	maxKDFMemory   = 256 * 1024 // KiB
	maxKDFTime     = 4
	maxKDFThreads  = 16
	maxKeyFileSize = 64 << 10
)

// ErrTooManyKeyFiles: none of the key files tried fit the password, but
// more were in the storage than an open tries (planted ones?). It is an
// ErrWrongPassword, so callers that try several keys go on.
var ErrTooManyKeyFiles = fmt.Errorf("%w; the repository has more key files than are tried (check its keys folder for files that do not belong there)", ErrWrongPassword)

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
	var loadErr error
	// A repository has a few key files; planted ones must not make every
	// open read thousands of files or try many key derivations. Only valid
	// ones count toward the derivations, so invalid planted files cannot
	// push the real key out (L-N11), and files with BackupZit's own
	// parameters are tried first, so planted files with other (still
	// allowed) parameters cannot either.
	more := len(names) > maxKeyListing
	if more {
		names = names[:maxKeyListing]
	}
	type candidate struct {
		name string
		kf   keyFile
	}
	var own, other []candidate
	for _, n := range names {
		b, err := backend.LoadMax(ctx, be, n, maxKeyFileSize)
		if errors.Is(err, backend.ErrTooLarge) {
			continue
		}
		if err != nil {
			loadErr = err
			continue
		}
		var kf keyFile
		// argon2 panics on 0 rounds or threads, which would end the agent
		// or the console; huge costs would exhaust its memory and time.
		if len(b) > maxKeyFileSize || json.Unmarshal(b, &kf) != nil || kf.KDF != "argon2id" ||
			kf.Memory < 8 || kf.Memory > maxKDFMemory || kf.Time < 1 || kf.Time > maxKDFTime || kf.Threads < 1 || kf.Threads > maxKDFThreads {
			continue
		}
		if kf.Time == kdfTime && kf.Memory == kdfMemory && kf.Threads == kdfThreads {
			own = append(own, candidate{n, kf})
		} else {
			other = append(other, candidate{n, kf})
		}
	}
	all := append(own, other...)
	if len(all) > maxKeyFiles {
		all, more = all[:maxKeyFiles], true
	}
	for _, c := range all {
		kf := c.kf
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
			return nil, fmt.Errorf("key file %s is damaged", c.name)
		}
		return &mk, nil
	}
	if loadErr != nil {
		// A storage error is not a wrong recovery key.
		return nil, fmt.Errorf("read key files: %w", loadErr)
	}
	if more {
		return nil, ErrTooManyKeyFiles
	}
	return nil, ErrWrongPassword
}
