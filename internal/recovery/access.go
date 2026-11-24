package recovery

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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

// Limits on the files the recovery side reads. A package is small: about
// 1 to 4 KiB per repository, most of it an SFTP key. The limits keep a
// damaged or planted file, or a folder full of them, from taking the
// memory or the time of a recovery machine before anything is chosen.
const (
	// MaxAccessPackageSize is the largest access package that is read or
	// written (several hundred repositories). It is checked before a file
	// is read.
	MaxAccessPackageSize = 1 << 20
	// maxAccessPackagesPerDrive and maxAccessPackagesTotal bound how many
	// \backupzit\*.bzaccess files FindAccessPackages reads, so at most
	// 64 MiB in all however many files or drives there are.
	maxAccessPackagesPerDrive = 16
	maxAccessPackagesTotal    = 64
)

// Errors for files that are not, or no longer, a readable access package.
// They are told apart from ErrAccessPassphrase because the header is in
// the clear: saying what is wrong with it reveals nothing.
var (
	ErrNotAccessPackage      = errors.New("not a BackupZit access package")
	ErrAccessDamaged         = errors.New("damaged access package")
	ErrAccessPackageTooLarge = fmt.Errorf("access package larger than %d KiB", MaxAccessPackageSize>>10)
)

// ErrAccessPassphrase is returned for a wrong passphrase or a damaged or
// manipulated file (they cannot be told apart).
var ErrAccessPassphrase = errors.New("wrong passphrase, or the access package is damaged")

// AccessRepo is one repository in an access package.
type AccessRepo struct {
	Name     string `json:"name"`
	Location string `json:"location"`
	Password string `json:"password,omitempty"` // repository password or recovery key
	// RepoID is the repository's ID (the "id" in its config), when it was
	// known when the package was made. With it the recovery side tells
	// repositories at the same path apart; packages without it work as
	// before.
	RepoID string `json:"repo_id,omitempty"`

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

// Source turns the entry into a Source. A local path whose drive letter
// changed is found on the other drives, by the repository ID when the
// package has one. An SFTP key is written to a file in tempDir (the
// backend reads keys from files); cleanup removes it.
func (a AccessRepo) Source(tempDir string) (src Source, cleanup func(), err error) {
	return a.source(tempDir, DriveRoots())
}

// source is Source with the drive roots to search (tests use folders).
func (a AccessRepo) source(tempDir string, roots []string) (src Source, cleanup func(), err error) {
	cleanup = func() {}
	loc := LocateLocalRepo(a.Location, roots)
	if a.RepoID != "" {
		if loc, err = LocateLocalRepoID(a.Location, a.RepoID, roots); err != nil {
			return src, cleanup, err
		}
	}
	src = Source{Location: loc, RepoID: a.RepoID, Password: a.Password, Options: backend.Options{
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

// aad is the header in canonical form (the envelope without nonce and
// ciphertext, marshalled again), which the ciphertext is bound to. Both
// sides marshal the parsed struct, so the bytes of the file do not matter.
func (e *accessEnvelope) aad() ([]byte, error) {
	c := *e
	c.Nonce, c.Ciphertext = nil, nil
	b, err := json.Marshal(c)
	if err != nil {
		// Only a time that cannot be written again (a year beyond 9999)
		// gets here; without an error check the header would go unbound.
		return nil, fmt.Errorf("%w: %v", ErrAccessDamaged, err)
	}
	return b, nil
}

// Argon2id parameters written into new packages: RFC 9106's second
// recommendation, with 64 MiB because Windows PE machines may have little
// memory.
const (
	accessKDFTime    = 3
	accessKDFMemory  = 64 * 1024 // KiB
	accessKDFThreads = 4
)

// Bounds on the parameters a package may ask for. They are in the clear,
// so without bounds a damaged or crafted package demands any memory and
// time before the passphrase is even checked, or crashes the program.
//   - Time 1 to 4: argon2.IDKey panics at 0; 4 leaves room above the 3
//     written.
//   - Memory up to 256 MiB, and at least the 8 KiB per thread Argon2
//     defines (below that argon2 silently raises it). Windows PE runs from
//     a RAM disk and takes about 0.5 to 1 GB itself; the machines it
//     recovers ran 64-bit Windows 10 or 11, which need at least 2 GB.
//     256 MiB still leaves PE and the restore room on such a machine, and
//     at four times the 64 MiB written, later packages can be made
//     stronger without a new format.
//   - Threads 1 to 16.
//   - Salt 16 to 64 bytes (16 written); nonce exactly the AES-GCM size.
//
// The worst case allowed (4 passes over 256 MiB) costs about five times
// the parameters written (3 over 64 MiB): around a second, not minutes.
const (
	accessKDFMinTime    = 1
	accessKDFMaxTime    = 4
	accessKDFMaxMemory  = 256 * 1024 // KiB
	accessKDFMaxThreads = 16
	accessMinSalt       = 16
	accessMaxSalt       = 64
	accessNonceSize     = 12
	accessTagSize       = 16
)

// checkKDF refuses key derivation parameters outside the bounds above.
// accessAEAD calls it too, so argon2 never runs with parameters that were
// not checked.
func checkKDF(e *accessEnvelope) error {
	if e.KDF != "argon2id" {
		return fmt.Errorf("%w: unsupported key derivation %q", ErrAccessDamaged, e.KDF)
	}
	switch {
	case e.Time < accessKDFMinTime || e.Time > accessKDFMaxTime:
		return fmt.Errorf("%w: argon2id time %d is outside %d to %d", ErrAccessDamaged, e.Time, accessKDFMinTime, accessKDFMaxTime)
	case e.Threads < 1 || e.Threads > accessKDFMaxThreads:
		return fmt.Errorf("%w: argon2id threads %d is outside 1 to %d", ErrAccessDamaged, e.Threads, accessKDFMaxThreads)
	case e.MemoryKiB < 8*uint32(e.Threads) || e.MemoryKiB > accessKDFMaxMemory:
		return fmt.Errorf("%w: argon2id memory %d KiB is outside %d to %d KiB", ErrAccessDamaged, e.MemoryKiB, 8*uint32(e.Threads), accessKDFMaxMemory)
	case len(e.Salt) < accessMinSalt || len(e.Salt) > accessMaxSalt:
		return fmt.Errorf("%w: salt of %d bytes, not %d to %d", ErrAccessDamaged, len(e.Salt), accessMinSalt, accessMaxSalt)
	}
	return nil
}

// accessAEAD derives the package key from the passphrase and returns the
// AES-256-GCM cipher for it.
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

// SealAccessPackage encrypts p with passphrase, which must pass
// CheckPassphrase. A result larger than MaxAccessPackageSize is refused:
// the recovery side would not read it.
func SealAccessPackage(p *AccessPackage, passphrase string) ([]byte, error) {
	if err := CheckPassphrase(passphrase); err != nil {
		return nil, err
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
	// Base64 makes the ciphertext a third larger than plain; refuse before
	// the key derivation when it cannot fit.
	if len(plain) > MaxAccessPackageSize*3/4 {
		return nil, ErrAccessPackageTooLarge
	}
	e := &accessEnvelope{Format: accessFormat, Version: 1, Created: p.Created, KDF: "argon2id",
		Salt: make([]byte, accessMinSalt), Time: accessKDFTime, MemoryKiB: accessKDFMemory, Threads: accessKDFThreads}
	if _, err := rand.Read(e.Salt); err != nil {
		return nil, err
	}
	a, err := accessAEAD(passphrase, e)
	if err != nil {
		return nil, err
	}
	aad, err := e.aad()
	if err != nil {
		return nil, err
	}
	e.Nonce = make([]byte, a.NonceSize())
	if _, err := rand.Read(e.Nonce); err != nil {
		return nil, err
	}
	e.Ciphertext = a.Seal(nil, e.Nonce, plain, aad)
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return nil, err
	}
	if len(b) > MaxAccessPackageSize {
		return nil, ErrAccessPackageTooLarge
	}
	return b, nil
}

// AccessPackageInfo is what can be read without the passphrase.
type AccessPackageInfo struct {
	Path    string
	Created time.Time
}

// parseEnvelope parses and checks the envelope of an access package: size,
// format, version, key derivation parameters, nonce and ciphertext length.
// Everything that can be refused without the passphrase is refused here,
// before the costly key derivation, with an error that says why.
func parseEnvelope(b []byte) (*accessEnvelope, error) {
	if len(b) > MaxAccessPackageSize {
		return nil, ErrAccessPackageTooLarge
	}
	var e accessEnvelope
	err := json.Unmarshal(b, &e)
	if e.Format != accessFormat {
		return nil, ErrNotAccessPackage
	}
	if err != nil {
		// The format is ours but a field is not what it should be (a
		// number too large for its type, a salt that is not base64).
		return nil, fmt.Errorf("%w: %v", ErrAccessDamaged, err)
	}
	if e.Version != 1 {
		return nil, fmt.Errorf("unsupported access package version %d", e.Version)
	}
	if err := checkKDF(&e); err != nil {
		return nil, err
	}
	if len(e.Nonce) != accessNonceSize {
		return nil, fmt.Errorf("%w: nonce of %d bytes, not %d", ErrAccessDamaged, len(e.Nonce), accessNonceSize)
	}
	if len(e.Ciphertext) < accessTagSize {
		return nil, fmt.Errorf("%w: ciphertext of %d bytes", ErrAccessDamaged, len(e.Ciphertext))
	}
	if _, err := e.aad(); err != nil {
		return nil, err
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
	return openEnvelope(e, a)
}

// openEnvelope decrypts a parsed envelope with the derived cipher and
// parses the content.
func openEnvelope(e *accessEnvelope, a cipher.AEAD) (*AccessPackage, error) {
	if len(e.Nonce) != a.NonceSize() {
		return nil, ErrAccessPassphrase
	}
	aad, err := e.aad()
	if err != nil {
		return nil, err
	}
	plain, err := a.Open(nil, e.Nonce, e.Ciphertext, aad)
	if err != nil {
		return nil, ErrAccessPassphrase
	}
	var p AccessPackage
	if err := json.Unmarshal(plain, &p); err != nil {
		return nil, fmt.Errorf("access package content: %w", err)
	}
	return &p, nil
}

// ReadAccessPackageFile reads an access package file. Anything but a
// regular file of at most MaxAccessPackageSize is refused before it is
// read (a device or a pipe named *.bzaccess would never end), and the read
// itself stops at the limit in case the file grows meanwhile.
func ReadAccessPackageFile(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: %w (not a regular file)", path, ErrNotAccessPackage)
	}
	if fi.Size() > MaxAccessPackageSize {
		return nil, fmt.Errorf("%s: %w", path, ErrAccessPackageTooLarge)
	}
	f, err := os.Open(path) // #nosec G304 -- the user names the package, or it is in the fixed \backupzit folder
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, MaxAccessPackageSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxAccessPackageSize {
		return nil, fmt.Errorf("%s: %w", path, ErrAccessPackageTooLarge)
	}
	return b, nil
}

// FindAccessPackages looks for \backupzit\*.bzaccess below each root (the
// drive roots in the recovery environment), newest first. It runs as soon
// as the wizard opens, on every drive, including ones a stranger may have
// prepared: it reads at most maxAccessPackagesPerDrive files per drive (in
// name order) and maxAccessPackagesTotal in all, each only up to
// MaxAccessPackageSize, and skips what is not a valid package.
func FindAccessPackages(roots []string) []AccessPackageInfo {
	var out []AccessPackageInfo
	read := 0
	for _, r := range roots {
		m, _ := filepath.Glob(filepath.Join(r, "backupzit", "*"+AccessPackageExt))
		if len(m) > maxAccessPackagesPerDrive {
			m = m[:maxAccessPackagesPerDrive]
		}
		for _, p := range m {
			if read >= maxAccessPackagesTotal {
				break
			}
			read++
			b, err := ReadAccessPackageFile(p)
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

// --- Passphrases ----------------------------------------------------------

// Passphrase lengths: MinPassphraseLength with three or more kinds of
// characters (lower case, upper case, digits, others), the longer ones
// with fewer kinds. Each step makes up for the smaller alphabet.
const (
	MinPassphraseLength         = 12
	MinPassphraseLengthTwoKinds = 16
	MinPassphraseLengthOneKind  = 20
)

// Reasons CheckPassphrase refuses a passphrase. The console shows its own
// translated text for each.
var (
	ErrPassphraseShort       = fmt.Errorf("the passphrase must have at least %d characters", MinPassphraseLength)
	ErrPassphraseControl     = errors.New("the passphrase contains control characters")
	ErrPassphraseSpace       = errors.New("the passphrase must not begin or end with a space")
	ErrPassphraseCommon      = errors.New("the passphrase is a common password or made from one")
	ErrPassphraseKinds       = fmt.Errorf("the passphrase needs more kinds of characters or more length: at least %d characters with three of lower case, upper case, digits and others, %d with two kinds, %d with one", MinPassphraseLength, MinPassphraseLengthTwoKinds, MinPassphraseLengthOneKind)
	ErrPassphrasePredictable = errors.New("the passphrase is too predictable: it repeats characters or follows sequences such as aaaa, 1234 or qwerty")
)

// CheckPassphrase reports why passphrase is too weak to protect an access
// package, or nil. A package holds the credentials of every chosen storage
// and the repository keys, and it travels on USB sticks and in ISO images,
// where it can be copied and attacked offline: Argon2id makes each guess
// slow, but it does not save a passphrase that is in every guessing list.
// The rules are simple enough to explain next to the field:
//   - at least MinPassphraseLength characters, no control characters, and
//     no space at either end (invisible when it is typed again);
//   - not a common password, also not one dressed up with digits, symbols
//     or digits for letters ("P@ssw0rd2026!");
//   - MinPassphraseLength characters with three or more kinds,
//     MinPassphraseLengthTwoKinds with two, MinPassphraseLengthOneKind with
//     one;
//   - the same length again when only the characters count that do not
//     just repeat or continue what came before (aaaa, abcd, 4321, qwerty,
//     a group typed twice).
//
// Existing packages are not checked when they are opened.
func CheckPassphrase(passphrase string) error {
	if !utf8.ValidString(passphrase) || strings.IndexFunc(passphrase, unicode.IsControl) >= 0 {
		return ErrPassphraseControl
	}
	if strings.TrimSpace(passphrase) != passphrase {
		return ErrPassphraseSpace
	}
	rs := []rune(passphrase)
	if len(rs) < MinPassphraseLength {
		return ErrPassphraseShort
	}
	if commonPassphrase(passphrase) {
		return ErrPassphraseCommon
	}
	need := lengthForKinds(characterKinds(rs))
	if len(rs) < need {
		return ErrPassphraseKinds
	}
	if surprisingRunes(rs) < need {
		return ErrPassphrasePredictable
	}
	return nil
}

// characterKinds counts the kinds of characters in rs: lower case, upper
// case, digits and everything else (spaces, symbols, letters without case).
func characterKinds(rs []rune) int {
	var kinds [4]bool
	for _, r := range rs {
		switch {
		case unicode.IsLower(r):
			kinds[0] = true
		case unicode.IsUpper(r):
			kinds[1] = true
		case unicode.IsDigit(r):
			kinds[2] = true
		default:
			kinds[3] = true
		}
	}
	n := 0
	for _, k := range kinds {
		if k {
			n++
		}
	}
	return n
}

// lengthForKinds is the length a passphrase with that many kinds of
// characters needs.
func lengthForKinds(kinds int) int {
	switch {
	case kinds >= 3:
		return MinPassphraseLength
	case kinds == 2:
		return MinPassphraseLengthTwoKinds
	}
	return MinPassphraseLengthOneKind
}

// keyboardRows are runs of neighbouring keys on QWERTY, QWERTZ and AZERTY
// keyboards and the digit row: typing along them is a sequence too.
var keyboardRows = []string{"1234567890-=", "qwertyuiop[]", "qwertzuiopü+", "azertyuiop", "asdfghjkl;'", "asdfghjklöä", "qsdfghjklm", "zxcvbnm,./", "yxcvbnm,.-", "wxcvbn,;:!"}

// surprisingRunes counts the characters of rs that a guesser could not
// predict from the ones before: not a repeat of the previous character, not
// the next step of a sequence (abc, 321, qwe), and not the end of a group
// of three that came earlier already (the third "Ab1" in "Ab1Ab1Ab1").
// Case is ignored.
func surprisingRunes(rs []rune) int {
	low := make([]rune, len(rs))
	for i, r := range rs {
		low[i] = unicode.ToLower(r)
	}
	seen := map[[3]rune]bool{}
	n := 0
	for i, r := range low {
		predictable := i >= 1 && r == low[i-1]
		if i >= 2 {
			tri := [3]rune{low[i-2], low[i-1], r}
			predictable = predictable || isStep(low[i-2], low[i-1], r) || seen[tri]
			seen[tri] = true
		}
		if !predictable {
			n++
		}
	}
	return n
}

// isStep reports whether c continues the sequence a, b by the same step of
// one: the next letter or digit up or down (abc, 987), or the next key
// along a keyboard row (qwe, lkj).
func isStep(a, b, c rune) bool {
	if d := b - a; (d == 1 || d == -1) && c-b == d {
		return true
	}
	for _, row := range keyboardRows {
		i, j, k := strings.IndexRune(row, a), strings.IndexRune(row, b), strings.IndexRune(row, c)
		if i < 0 || j < 0 || k < 0 {
			continue
		}
		if d := j - i; (d == 1 || d == -1) && k-j == d {
			return true
		}
	}
	return false
}

// commonPasswords are passwords and words every guessing list starts with,
// in the languages of the console, plus the product's own names (lower
// case). Sorted longest first in init, so that "qwertyuiop" goes before
// "qwerty".
var commonPasswords = []string{
	"password", "passwort", "passwd", "motdepasse", "wachtwoord", "contrasena", "contraseña", "senha", "parola",
	"haslo", "hasło", "heslo", "lozinka", "sifra", "šifra", "sifre", "şifre", "parol", "пароль", "mima", "kennwort",
	"geheim", "secret", "qwerty", "qwertz", "azerty", "asdfgh", "yxcvbn", "zxcvbn", "qwertyuiop", "qwertzuiop",
	"azertyuiop", "asdfghjkl", "zxcvbnm", "yxcvbnm", "1qaz2wsx", "q1w2e3r4", "1q2w3e4r", "123456", "1234567890",
	"0987654321", "111111", "000000", "123123", "654321", "abc123", "letmein", "welcome", "willkommen",
	"bienvenue", "iloveyou", "ichliebedich", "admin", "administrator", "root", "toor", "guest", "user", "login",
	"default", "changeme", "master", "monkey", "dragon", "sunshine", "princess", "football", "fussball",
	"baseball", "soccer", "hockey", "shadow", "superman", "batman", "starwars", "pokemon", "trustno1", "whatever",
	"freedom", "hello", "hallo", "ciao", "summer", "winter", "spring", "autumn", "sommer", "herbst", "fruehling",
	"backup", "backups", "backupzit", "maxzit", "recovery", "restore", "windows", "microsoft", "server",
	"computer", "internet", "test", "testing", "demo", "access", "zugang", "company", "firma",
	"correcthorsebatterystaple",
}

func init() {
	sort.SliceStable(commonPasswords, func(i, j int) bool {
		return utf8.RuneCountInString(commonPasswords[i]) > utf8.RuneCountInString(commonPasswords[j])
	})
}

// minOwnPart is how many characters must remain once the common words are
// taken out of a passphrase for it not to count as made from them.
const minOwnPart = 6

// leetLetters are the digits and symbols typed for letters.
var leetLetters = map[rune]rune{'0': 'o', '1': 'i', '3': 'e', '4': 'a', '5': 's', '7': 't', '@': 'a', '$': 's', '!': 'i'}

// commonPassphrase reports whether p is mostly common passwords: once they
// are taken out, fewer than minOwnPart characters remain. It looks at two
// forms, both lower case without separators: as typed ("1234567890ab"),
// and without the digits and symbols around it and with digits read as
// letters ("P@ssw0rd2026!" is "password").
func commonPassphrase(p string) bool {
	low := strings.ToLower(p)
	isSep := func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune("-_.+", r) }
	squeezed := strings.Map(func(r rune) rune {
		if isSep(r) {
			return -1
		}
		return r
	}, low)
	core := strings.TrimFunc(low, func(r rune) bool { return !unicode.IsLetter(r) })
	core = strings.Map(func(r rune) rune {
		if isSep(r) {
			return -1
		}
		if l, ok := leetLetters[r]; ok {
			return l
		}
		return r
	}, core)
	for _, form := range []string{squeezed, core} {
		rest := form
		for _, w := range commonPasswords {
			rest = strings.ReplaceAll(rest, w, "")
		}
		if rest != form && utf8.RuneCountInString(rest) < minOwnPart {
			return true
		}
	}
	return false
}

// passphraseLetters are the letters generated passphrases use: the ones on
// the same key in QWERTY, QWERTZ and AZERTY layouts (a, m, q, w, y and z
// move between them, and digits need Shift on AZERTY). The recovery
// environment's keyboard layout may not match the keyboard; these letters
// come out right anyway.
const passphraseLetters = "bcdefghijklnoprstuvx"

// GeneratePassphrase returns a random passphrase of five groups of five
// letters from passphraseLetters, separated by spaces (the one separator
// on the same key everywhere): about 108 bits from crypto/rand. It always
// passes CheckPassphrase.
func GeneratePassphrase() (string, error) {
	const groups, letters = 5, 5
	n := len(passphraseLetters)
	// Bytes at or above limit are drawn again, so every letter is equally
	// likely (256 is not a multiple of 20).
	limit := 256 - 256%n
	for range 100 {
		var sb strings.Builder
		buf := make([]byte, 1)
		for g := range groups {
			if g > 0 {
				sb.WriteByte(' ')
			}
			for i := 0; i < letters; {
				if _, err := rand.Read(buf); err != nil {
					return "", err
				}
				if int(buf[0]) >= limit {
					continue
				}
				sb.WriteByte(passphraseLetters[int(buf[0])%n])
				i++
			}
		}
		// A random draw can still look like a sequence or a common word in
		// rare cases; draw again rather than offer one the form refuses.
		if p := sb.String(); CheckPassphrase(p) == nil {
			return p, nil
		}
	}
	return "", errors.New("cannot generate a passphrase")
}
