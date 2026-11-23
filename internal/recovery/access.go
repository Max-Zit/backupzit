package recovery

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
	"golang.org/x/crypto/argon2"
)

// Access packages hold the location and credentials of repositories,
// encrypted with a passphrase of their own, so that in an emergency only
// that passphrase has to be typed. There is no unencrypted form.

// AccessPackageExt is the file extension of access packages; the recovery
// environment looks for \backupzit\*.bzaccess on every drive.
const AccessPackageExt = ".bzaccess"

const accessFormat = "backupzit-access"

// MinPassphraseLength is the shortest passphrase an access package accepts.
const MinPassphraseLength = 12

// ErrAccessPassphrase is returned for a wrong passphrase or a damaged or
// manipulated file (they cannot be told apart).
var ErrAccessPassphrase = errors.New("wrong passphrase, or the access package is damaged")

// AccessRepo is one repository in an access package.
type AccessRepo struct {
	Name     string `json:"name"`
	Location string `json:"location"`
	Password string `json:"password,omitempty"` // repository password or recovery key

	SFTPPassword        string `json:"sftp_password,omitempty"`
	SFTPKey             string `json:"sftp_key,omitempty"` // private key contents (PEM)
	SFTPHostKey         string `json:"sftp_hostkey,omitempty"`
	S3AccessKey         string `json:"s3_access_key,omitempty"`
	S3SecretKey         string `json:"s3_secret_key,omitempty"`
	S3Region            string `json:"s3_region,omitempty"`
	SMBPassword         string `json:"smb_password,omitempty"`
	SMBDomain           string `json:"smb_domain,omitempty"`
	AzureKey            string `json:"azure_key,omitempty"`
	AzureSAS            string `json:"azure_sas,omitempty"`
	HardenedKey         string `json:"hardened_key,omitempty"`
	HardenedFingerprint string `json:"hardened_fingerprint,omitempty"`
}

// Source turns the entry into a Source. An SFTP key is written to a file
// in tempDir (the backend reads keys from files); cleanup removes it.
func (a AccessRepo) Source(tempDir string) (src Source, cleanup func(), err error) {
	cleanup = func() {}
	src = Source{Location: LocateLocalRepo(a.Location, DriveRoots()), Password: a.Password, Options: backend.Options{
		SFTPPassword: a.SFTPPassword, SFTPHostKey: a.SFTPHostKey,
		S3AccessKey: a.S3AccessKey, S3SecretKey: a.S3SecretKey, S3Region: a.S3Region,
		SMBPassword: a.SMBPassword, SMBDomain: a.SMBDomain,
		AzureKey: a.AzureKey, AzureSAS: a.AzureSAS,
		HardenedKey: a.HardenedKey, HardenedFingerprint: a.HardenedFingerprint,
	}}
	if a.SFTPKey != "" {
		f, err := os.CreateTemp(tempDir, "bz-sftp-key-*")
		if err != nil {
			return src, cleanup, err
		}
		name := f.Name()
		cleanup = func() { _ = os.Remove(name) }
		_, err = f.WriteString(a.SFTPKey)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			cleanup()
			return src, func() {}, err
		}
		src.Options.SFTPKeyFile = name
	}
	return src, cleanup, nil
}

// AccessPackage is the decrypted content of an access package.
type AccessPackage struct {
	Created      time.Time    `json:"created"`
	CreatedBy    string       `json:"created_by,omitempty"`
	Repositories []AccessRepo `json:"repositories"`
}

// accessEnvelope is the file format: KDF parameters and the encrypted
// package. The header is bound to the ciphertext as additional data, so
// changing a parameter makes decryption fail.
type accessEnvelope struct {
	Format     string    `json:"format"`
	Version    int       `json:"version"`
	Created    time.Time `json:"created"` // also inside; here readable without the passphrase
	KDF        string    `json:"kdf"`
	Salt       []byte    `json:"salt"`
	Time       uint32    `json:"time"`
	MemoryKiB  uint32    `json:"memory_kib"`
	Threads    uint8     `json:"threads"`
	Nonce      []byte    `json:"nonce"`
	Ciphertext []byte    `json:"ciphertext"`
}

func (e *accessEnvelope) aad() []byte {
	c := *e
	c.Nonce, c.Ciphertext = nil, nil
	b, _ := json.Marshal(c)
	return b
}

// Argon2id parameters; WinPE machines may have little memory, so 64 MiB.
const (
	accessKDFTime    = 3
	accessKDFMemory  = 64 * 1024
	accessKDFThreads = 4
)

// checkKDF refuses key derivation parameters far beyond the ones written
// here (64 MiB, 3 passes, 4 threads): a damaged or crafted package must not
// take the memory of a recovery machine (Windows PE often has 2-4 GB).
func checkKDF(e *accessEnvelope) error {
	if e.KDF != "argon2id" {
		return fmt.Errorf("unsupported key derivation %q", e.KDF)
	}
	if e.MemoryKiB > 1024*1024 || e.Time > 10 || e.Threads == 0 || e.Threads > 16 || len(e.Salt) < 16 {
		return ErrAccessPassphrase
	}
	return nil
}

func accessAEAD(passphrase string, e *accessEnvelope) (cipher.AEAD, error) {
	if err := checkKDF(e); err != nil {
		return nil, err
	}
	key := argon2.IDKey([]byte(passphrase), e.Salt, e.Time, e.MemoryKiB, e.Threads, 32)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// SealAccessPackage encrypts p with passphrase.
func SealAccessPackage(p *AccessPackage, passphrase string) ([]byte, error) {
	if len([]rune(passphrase)) < MinPassphraseLength {
		return nil, fmt.Errorf("the passphrase must have at least %d characters", MinPassphraseLength)
	}
	if len(p.Repositories) == 0 {
		return nil, errors.New("an access package needs at least one repository")
	}
	if p.Created.IsZero() {
		p.Created = time.Now().UTC()
	}
	plain, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	e := &accessEnvelope{Format: accessFormat, Version: 1, Created: p.Created, KDF: "argon2id",
		Salt: make([]byte, 16), Time: accessKDFTime, MemoryKiB: accessKDFMemory, Threads: accessKDFThreads}
	if _, err := rand.Read(e.Salt); err != nil {
		return nil, err
	}
	a, err := accessAEAD(passphrase, e)
	if err != nil {
		return nil, err
	}
	e.Nonce = make([]byte, a.NonceSize())
	if _, err := rand.Read(e.Nonce); err != nil {
		return nil, err
	}
	e.Ciphertext = a.Seal(nil, e.Nonce, plain, e.aad())
	return json.MarshalIndent(e, "", "  ")
}

// AccessPackageInfo is what can be read without the passphrase.
type AccessPackageInfo struct {
	Path    string
	Created time.Time
}

func parseEnvelope(b []byte) (*accessEnvelope, error) {
	var e accessEnvelope
	if err := json.Unmarshal(b, &e); err != nil || e.Format != accessFormat {
		return nil, errors.New("not a BackupZit access package")
	}
	if e.Version != 1 {
		return nil, fmt.Errorf("unsupported access package version %d", e.Version)
	}
	return &e, nil
}

// OpenAccessPackage decrypts an access package.
func OpenAccessPackage(b []byte, passphrase string) (*AccessPackage, error) {
	e, err := parseEnvelope(b)
	if err != nil {
		return nil, err
	}
	a, err := accessAEAD(passphrase, e)
	if err != nil {
		return nil, err
	}
	if len(e.Nonce) != a.NonceSize() {
		return nil, ErrAccessPassphrase
	}
	plain, err := a.Open(nil, e.Nonce, e.Ciphertext, e.aad())
	if err != nil {
		return nil, ErrAccessPassphrase
	}
	var p AccessPackage
	if err := json.Unmarshal(plain, &p); err != nil {
		return nil, fmt.Errorf("access package content: %w", err)
	}
	return &p, nil
}

// FindAccessPackages looks for \backupzit\*.bzaccess below each root (the
// drive roots in the recovery environment), newest first.
func FindAccessPackages(roots []string) []AccessPackageInfo {
	var out []AccessPackageInfo
	for _, r := range roots {
		m, _ := filepath.Glob(filepath.Join(r, "backupzit", "*"+AccessPackageExt))
		for _, p := range m {
			b, err := os.ReadFile(p) // #nosec G304 -- fixed folder on local drives
			if err != nil {
				continue
			}
			if e, err := parseEnvelope(b); err == nil {
				out = append(out, AccessPackageInfo{Path: p, Created: e.Created})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

// DriveRoots returns the roots of the drives to search (C:\ to Z:\ on
// Windows, except the recovery environment's own X:\; / elsewhere).
func DriveRoots() []string {
	if filepath.Separator != '\\' {
		return []string{"/"}
	}
	var out []string
	for d := 'C'; d <= 'Z'; d++ {
		if d == 'X' && strings.EqualFold(os.Getenv("SystemDrive"), "X:") {
			continue
		}
		root := string(d) + `:\`
		if _, err := os.Stat(root); err == nil {
			out = append(out, root)
		}
	}
	return out
}
