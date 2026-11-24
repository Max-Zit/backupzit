package recovery

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testPassphrase = "correct horse battery"

// Secrets in the test package. Each one is long and contains a character
// that base64 never produces ('-', '@', '.', ' '), so finding it in the
// sealed file means it is there in plain text and never that the random
// salt, nonce or ciphertext happened to spell it. (A three-letter marker
// such as "nas" turned up by chance in about 1 of 330 sealed files, which
// made TestAccessPackageRoundTrip fail now and then.)
const (
	testRepoSecret = "repo-secret-6c1f"
	testSMBSecret  = "smb-secret-93ad"
	testHost       = "nas-7f3c.example"
	testSFTPKey    = "-----BEGIN OPENSSH PRIVATE KEY-----\nxyz\n-----END OPENSSH PRIVATE KEY-----\n"
)

func testPackage() *AccessPackage {
	return &AccessPackage{CreatedBy: "admin", Repositories: []AccessRepo{
		{Name: "pc1", Location: "smb://backup@" + testHost + "/backups/pc1", Password: testRepoSecret, SMBPassword: testSMBSecret},
		{Name: "pc2", Location: "sftp://bz@host/repo", SFTPKey: testSFTPKey},
	}}
}

func TestAccessPackageRoundTrip(t *testing.T) {
	b, err := SealAccessPackage(testPackage(), testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	var e accessEnvelope
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{testRepoSecret, testSMBSecret, testHost, "-----BEGIN OPENSSH"} {
		if strings.Contains(string(b), secret) {
			t.Errorf("file contains %q in plain text", secret)
		}
		if bytes.Contains(e.Ciphertext, []byte(secret)) {
			t.Errorf("ciphertext contains %q", secret)
		}
	}
	p, err := OpenAccessPackage(b, testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Repositories) != 2 || p.Repositories[0].SMBPassword != testSMBSecret || p.Repositories[1].SFTPKey != testSFTPKey || p.Created.IsZero() {
		t.Errorf("decrypted %+v", p)
	}
	if _, err := OpenAccessPackage(b, "wrong passphrase!"); !errors.Is(err, ErrAccessPassphrase) {
		t.Errorf("wrong passphrase: %v", err)
	}
}

func TestAccessPackageTampering(t *testing.T) {
	b, err := SealAccessPackage(testPackage(), testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	var e map[string]any
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(m map[string]any){
		func(m map[string]any) { m["created"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano) },
		func(m map[string]any) { m["time"] = 1.0 },
		func(m map[string]any) {
			c := []byte(m["ciphertext"].(string))
			c[10] ^= 1
			m["ciphertext"] = string(c)
		},
	} {
		m := map[string]any{}
		for k, v := range e {
			m[k] = v
		}
		change(m)
		tb, _ := json.Marshal(m)
		if _, err := OpenAccessPackage(tb, testPassphrase); err == nil {
			t.Errorf("manipulated package opened: %s", tb)
		}
	}
	if _, err := OpenAccessPackage([]byte(`{"format":"other"}`), testPassphrase); !errors.Is(err, ErrNotAccessPackage) {
		t.Errorf("foreign file: %v", err)
	}
}

func TestAccessPackageRules(t *testing.T) {
	if _, err := SealAccessPackage(testPackage(), "short"); !errors.Is(err, ErrPassphraseShort) {
		t.Errorf("short passphrase: %v", err)
	}
	if _, err := SealAccessPackage(testPackage(), "aaaaaaaaaaaaaaaaaaaaaaaa"); !errors.Is(err, ErrPassphrasePredictable) {
		t.Errorf("repeated passphrase: %v", err)
	}
	if _, err := SealAccessPackage(&AccessPackage{}, testPassphrase); err == nil {
		t.Error("empty package accepted")
	}
}

// A package the recovery side would not read is not written: a repository
// so large that the file passes MaxAccessPackageSize is refused, before
// the key derivation.
func TestSealAccessPackageTooLarge(t *testing.T) {
	p := testPackage()
	p.Repositories[1].SFTPKey = strings.Repeat("k", MaxAccessPackageSize*3/4)
	if _, err := SealAccessPackage(p, testPassphrase); !errors.Is(err, ErrAccessPackageTooLarge) {
		t.Errorf("oversized package: %v", err)
	}
}

// testEnvelope is a structurally valid package with the parameters this
// code writes and random bytes as ciphertext: enough for everything that
// is checked without the passphrase, and without the cost of Argon2id.
func testEnvelope(t testing.TB, created time.Time, change func(e *accessEnvelope)) []byte {
	t.Helper()
	e := accessEnvelope{Format: accessFormat, Version: 1, Created: created, KDF: "argon2id",
		Salt: make([]byte, 16), Time: accessKDFTime, MemoryKiB: accessKDFMemory, Threads: accessKDFThreads,
		Nonce: make([]byte, 12), Ciphertext: make([]byte, 64)}
	_, _ = rand.Read(e.Salt)
	_, _ = rand.Read(e.Ciphertext)
	if change != nil {
		change(&e)
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFindAccessPackages(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "backupzit")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := testPackage()
	old.Created = time.Now().Add(-48 * time.Hour).UTC()
	for name, p := range map[string]*AccessPackage{"old.bzaccess": old, "new.bzaccess": testPackage()} {
		b, err := SealAccessPackage(p, testPassphrase)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "junk.bzaccess"), []byte("nope"), 0o600)
	// A package with key derivation parameters it may not ask for is not
	// offered at all.
	_ = os.WriteFile(filepath.Join(dir, "time0.bzaccess"), testEnvelope(t, time.Now(), func(e *accessEnvelope) { e.Time = 0 }), 0o600)
	got := FindAccessPackages([]string{root, filepath.Join(root, "missing")})
	if len(got) != 2 || filepath.Base(got[0].Path) != "new.bzaccess" {
		t.Errorf("found %+v", got)
	}
}

// FindAccessPackages runs when the wizard opens, on every drive: a file
// larger than MaxAccessPackageSize is skipped without being read, and only
// so many files per drive and in all are read.
func TestFindAccessPackagesLimits(t *testing.T) {
	now := time.Now().UTC()
	var roots []string
	for d := 0; d < 6; d++ {
		root := t.TempDir()
		dir := filepath.Join(root, "backupzit")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < maxAccessPackagesPerDrive+4; i++ {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("p%02d.bzaccess", i)), testEnvelope(t, now, nil), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		roots = append(roots, root)
	}
	if got := FindAccessPackages(roots[:1]); len(got) != maxAccessPackagesPerDrive {
		t.Errorf("one drive with %d packages: %d read, want %d", maxAccessPackagesPerDrive+4, len(got), maxAccessPackagesPerDrive)
	}
	if got := FindAccessPackages(roots); len(got) != maxAccessPackagesTotal {
		t.Errorf("six drives: %d read, want %d", len(got), maxAccessPackagesTotal)
	}

	// A valid header padded beyond the limit (JSON allows trailing spaces).
	root := t.TempDir()
	dir := filepath.Join(root, "backupzit")
	_ = os.MkdirAll(dir, 0o700)
	big := append(testEnvelope(t, now, nil), bytes.Repeat([]byte(" "), MaxAccessPackageSize)...)
	if err := os.WriteFile(filepath.Join(dir, "big.bzaccess"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := FindAccessPackages([]string{root}); len(got) != 0 {
		t.Errorf("oversized package offered: %+v", got)
	}
	if _, err := ReadAccessPackageFile(filepath.Join(dir, "big.bzaccess")); !errors.Is(err, ErrAccessPackageTooLarge) {
		t.Errorf("oversized file read: %v", err)
	}
	// A directory (or a device, a pipe) named like a package is not read.
	_ = os.Mkdir(filepath.Join(dir, "folder.bzaccess"), 0o700)
	if _, err := ReadAccessPackageFile(filepath.Join(dir, "folder.bzaccess")); !errors.Is(err, ErrNotAccessPackage) {
		t.Errorf("directory read as package: %v", err)
	}
	if _, err := OpenAccessPackage(big, testPassphrase); !errors.Is(err, ErrAccessPackageTooLarge) {
		t.Errorf("oversized bytes opened: %v", err)
	}
}

func TestAccessRepoSourceOpensRepository(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "repo")
	newImageRepo(t, dir, "repo-pw")
	a := AccessRepo{Name: "pc", Location: dir, Password: "repo-pw", SFTPKey: "KEY"}
	src, cleanup, err := a.Source(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(src.Options.SFTPKeyFile); err != nil || string(b) != "KEY" {
		t.Errorf("key file: %q, %v", b, err)
	}
	s, err := Open(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	cleanup()
	if _, err := os.Stat(src.Options.SFTPKeyFile); !os.IsNotExist(err) {
		t.Error("key file not removed")
	}
}

// With the repository ID in the package, the recovery side tells two
// repositories at the same path apart (S10): two rotating USB disks with
// the same folder and password, the wanted one on the later drive.
func TestAccessRepoIDTellsRepositoriesApart(t *testing.T) {
	if filepath.Separator != '\\' {
		t.Skip("drive letters are Windows only")
	}
	ctx := context.Background()
	r1, r2 := t.TempDir(), t.TempDir()
	older, wanted := filepath.Join(r1, "backups", "pc"), filepath.Join(r2, "backups", "pc")
	newImageRepo(t, older, "repo-pw")
	newImageRepo(t, wanted, "repo-pw")
	id := repoIDAt(wanted)
	if id == "" || id == repoIDAt(older) {
		t.Fatalf("repository IDs %q and %q", id, repoIDAt(older))
	}
	roots := []string{r1, r2}

	a := AccessRepo{Name: "pc", Location: `Q:\backups\pc`, Password: "repo-pw", RepoID: id}
	src, _, err := a.source(t.TempDir(), roots)
	if err != nil || src.Location != wanted || src.RepoID != id {
		t.Fatalf("with ID: %q, %v; want %q", src.Location, err, wanted)
	}
	s, err := Open(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if s.Repo.Config().ID != id {
		t.Errorf("opened %s", s.Repo.Config().ID)
	}
	_ = s.Close()

	// A package made before the ID was recorded works as before: the first
	// drive with the path.
	a.RepoID = ""
	if src, _, err := a.source(t.TempDir(), roots); err != nil || src.Location != older {
		t.Errorf("without ID: %q, %v; want %q", src.Location, err, older)
	}

	// Neither repository is the one in the package: a clear error naming
	// what was found, nothing opened.
	a.RepoID = strings.Repeat("ab", 32)
	_, _, err = a.source(t.TempDir(), roots)
	if !errors.Is(err, ErrOtherRepository) || !strings.Contains(err.Error(), older) || !strings.Contains(err.Error(), wanted) {
		t.Errorf("unknown ID: %v", err)
	}
	// Nothing at the path anywhere: the path stays, and opening it says so.
	a.Location = `Q:\backups\missing`
	if src, _, err := a.source(t.TempDir(), roots); err != nil || src.Location != a.Location {
		t.Errorf("missing: %q, %v", src.Location, err)
	}
}

// Open checks the ID again once the password has opened the repository:
// the config's ID is readable, and changeable, without it.
func TestOpenChecksRepoID(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "repo")
	newImageRepo(t, dir, "repo-pw")
	id := repoIDAt(dir)
	for _, want := range []string{"", id} {
		s, err := Open(ctx, Source{Location: dir, Password: "repo-pw", RepoID: want})
		if err != nil {
			t.Fatalf("ID %q: %v", want, err)
		}
		_ = s.Close()
	}
	if _, err := Open(ctx, Source{Location: dir, Password: "repo-pw", RepoID: strings.Repeat("0", 64)}); !errors.Is(err, ErrOtherRepository) {
		t.Errorf("other ID opened: %v", err)
	}
	// The ID survives the package round trip.
	b, err := SealAccessPackage(&AccessPackage{Repositories: []AccessRepo{{Name: "pc", Location: dir, RepoID: id}}}, testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := OpenAccessPackage(b, testPassphrase); err != nil || p.Repositories[0].RepoID != id {
		t.Errorf("round trip: %+v, %v", p, err)
	}
}

// Key derivation parameters outside the bounds are refused before any
// memory is taken: a damaged or crafted package must neither crash the
// recovery program (argon2 panics at time 0) nor exhaust the memory of a
// Windows PE machine.
func TestKDFLimits(t *testing.T) {
	ok := accessEnvelope{KDF: "argon2id", Salt: make([]byte, 16), Time: accessKDFTime, MemoryKiB: accessKDFMemory, Threads: accessKDFThreads}
	if err := checkKDF(&ok); err != nil {
		t.Fatalf("own parameters refused: %v", err)
	}
	for name, change := range map[string]func(e *accessEnvelope){
		"time 1":         func(e *accessEnvelope) { e.Time = 1 },
		"time 4":         func(e *accessEnvelope) { e.Time = 4 },
		"memory 256 MiB": func(e *accessEnvelope) { e.MemoryKiB = 256 * 1024 },
		"threads 16":     func(e *accessEnvelope) { e.Threads = 16 },
		"salt 64":        func(e *accessEnvelope) { e.Salt = make([]byte, 64) },
	} {
		e := ok
		change(&e)
		if err := checkKDF(&e); err != nil {
			t.Errorf("%s refused: %v", name, err)
		}
	}
	for name, change := range map[string]func(e *accessEnvelope){
		"time 0":              func(e *accessEnvelope) { e.Time = 0 },
		"time 5":              func(e *accessEnvelope) { e.Time = 5 },
		"memory 1 GiB":        func(e *accessEnvelope) { e.MemoryKiB = 1024 * 1024 },
		"memory 256 MiB + 1":  func(e *accessEnvelope) { e.MemoryKiB = 256*1024 + 1 },
		"memory below 8/lane": func(e *accessEnvelope) { e.MemoryKiB = 8*accessKDFThreads - 1 },
		"threads 17":          func(e *accessEnvelope) { e.Threads = 17 },
		"no threads":          func(e *accessEnvelope) { e.Threads = 0 },
		"short salt":          func(e *accessEnvelope) { e.Salt = make([]byte, 15) },
		"long salt":           func(e *accessEnvelope) { e.Salt = make([]byte, 65) },
		"other kdf":           func(e *accessEnvelope) { e.KDF = "scrypt" },
	} {
		e := ok
		change(&e)
		if err := checkKDF(&e); !errors.Is(err, ErrAccessDamaged) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Opening a package with time 0 gave "argon2: number of rounds too small"
// as a panic; now every bad header is a clear error.
func TestOpenAccessPackageBadHeader(t *testing.T) {
	now := time.Now().UTC()
	for name, tc := range map[string]struct {
		b    []byte
		want error
	}{
		"time 0":           {testEnvelope(t, now, func(e *accessEnvelope) { e.Time = 0 }), ErrAccessDamaged},
		"memory 1 GiB":     {testEnvelope(t, now, func(e *accessEnvelope) { e.MemoryKiB = 1 << 20 }), ErrAccessDamaged},
		"8 MiB salt":       {testEnvelope(t, now, func(e *accessEnvelope) { e.Salt = make([]byte, 8<<20) }), ErrAccessPackageTooLarge},
		"1 KiB salt":       {testEnvelope(t, now, func(e *accessEnvelope) { e.Salt = make([]byte, 1024) }), ErrAccessDamaged},
		"short nonce":      {testEnvelope(t, now, func(e *accessEnvelope) { e.Nonce = e.Nonce[:11] }), ErrAccessDamaged},
		"no ciphertext":    {testEnvelope(t, now, func(e *accessEnvelope) { e.Ciphertext = nil }), ErrAccessDamaged},
		"time too large":   {[]byte(`{"format":"backupzit-access","version":1,"kdf":"argon2id","time":4294967296}`), ErrAccessDamaged},
		"salt not base64":  {[]byte(`{"format":"backupzit-access","version":1,"kdf":"argon2id","salt":"%%%"}`), ErrAccessDamaged},
		"not JSON":         {[]byte("PK\x03\x04"), ErrNotAccessPackage},
		"JSON of another":  {[]byte(`{"format":"other","version":1}`), ErrNotAccessPackage},
		"empty":            {nil, ErrNotAccessPackage},
		"array":            {[]byte(`[1,2,3]`), ErrNotAccessPackage},
		"format is number": {[]byte(`{"format":1}`), ErrNotAccessPackage},
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: panic %v", name, r)
				}
			}()
			if _, err := OpenAccessPackage(tc.b, testPassphrase); !errors.Is(err, tc.want) {
				t.Errorf("%s: %v, want %v", name, err, tc.want)
			}
		}()
	}
	if _, err := OpenAccessPackage(testEnvelope(t, now, func(e *accessEnvelope) { e.Version = 2 }), testPassphrase); err == nil || !strings.Contains(err.Error(), "version 2") {
		t.Errorf("version 2: %v", err)
	}
	// A well-formed header with the wrong key is a wrong passphrase.
	if _, err := OpenAccessPackage(testEnvelope(t, now, func(e *accessEnvelope) { e.Time, e.MemoryKiB = 1, 64 }), testPassphrase); !errors.Is(err, ErrAccessPassphrase) {
		t.Errorf("random ciphertext: %v", err)
	}
}

func TestCheckPassphrase(t *testing.T) {
	refused := map[string]error{
		"":                           ErrPassphraseShort,
		"Tr0ub4dor&3":                ErrPassphraseShort,
		"            ":               ErrPassphraseSpace,
		" Leading space 2026":        ErrPassphraseSpace,
		"trailing Space 2026 ":       ErrPassphraseSpace,
		"tab\tinside the phrase 1A":  ErrPassphraseControl,
		"line\nbreak in the phrase":  ErrPassphraseControl,
		"invalid \xff utf-8 phrase":  ErrPassphraseControl,
		"Password1234":               ErrPassphraseCommon,
		"P@ssw0rd2026!":              ErrPassphraseCommon,
		"Summer2026!!!!":             ErrPassphraseCommon,
		"qwertyuiop12":               ErrPassphraseCommon,
		"123456789012":               ErrPassphraseCommon,
		"MyBackupZit2026!":           ErrPassphraseCommon,
		"Welcome to BackupZit":       ErrPassphraseCommon,
		"lozinka-lozinka":            ErrPassphraseCommon,
		"correcthorsebatterystapler": ErrPassphraseCommon,
		"aaaaaaaaaaaa":               ErrPassphraseKinds,
		"kdjfuehtnvbx":               ErrPassphraseKinds, // 12 lower case letters only
		"kdjfuehtnvbxpros":           ErrPassphraseKinds, // 16, still one kind
		"Kdjf8ehtn3bx":               nil,                // 12 with three kinds
		"aaaaaaaaaaaaaaaaaaaaaaaa":   ErrPassphrasePredictable,
		"abcdefghijklmnopqrstuvwx":   ErrPassphrasePredictable,
		"Ab1Ab1Ab1Ab1":               ErrPassphrasePredictable,
		"Aaaaaaaaaaaa1":              ErrPassphrasePredictable,
		"vbnm,./ 1qAx":               ErrPassphrasePredictable, // along the bottom row
		"hjkl;'Ab1Xy9":               ErrPassphrasePredictable, // along the middle row
		"9876543210zyxwvu":           ErrPassphrasePredictable,
	}
	for p, want := range refused {
		if err := CheckPassphrase(p); !errors.Is(err, want) || (want == nil) != (err == nil) {
			t.Errorf("%q: %v, want %v", p, err, want)
		}
	}
	for _, p := range []string{
		testPassphrase,
		"a long enough passphrase",
		"violetmoonlitharbourgate",
		"Sunshine over Belgrade 7",
		"welcome#8fK2!x9Lq",
		"Zgrada na Dorćolu 1934",
		"дождь над Невой 1703",
		"kdtrb pnuch fgsei vxcob jhnte",
	} {
		if err := CheckPassphrase(p); err != nil {
			t.Errorf("%q refused: %v", p, err)
		}
	}
}

// Generated passphrases are random, pass the rules, and use only the
// letters that sit on the same key in QWERTY, QWERTZ and AZERTY.
func TestGeneratePassphrase(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 300; i++ {
		p, err := GeneratePassphrase()
		if err != nil {
			t.Fatal(err)
		}
		if err := CheckPassphrase(p); err != nil {
			t.Fatalf("%q refused: %v", p, err)
		}
		groups := strings.Split(p, " ")
		if len(groups) != 5 {
			t.Fatalf("%q: %d groups", p, len(groups))
		}
		for _, g := range groups {
			if len(g) != 5 || strings.Trim(g, passphraseLetters) != "" {
				t.Fatalf("%q: group %q", p, g)
			}
		}
		if seen[p] {
			t.Fatalf("%q generated twice", p)
		}
		seen[p] = true
	}
	for _, c := range "amqwyz0123456789" {
		if strings.ContainsRune(passphraseLetters, c) {
			t.Errorf("%q moves between keyboard layouts", c)
		}
	}
}

// FuzzParseAccessEnvelope feeds the envelope parser what a stranger could
// put on a drive of the machine being recovered: whatever it accepts has
// parameters within the bounds, a header that binds again, and decrypting
// it with some key fails cleanly. Nothing may panic or allocate beyond the
// file size.
func FuzzParseAccessEnvelope(f *testing.F) {
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	f.Add(testEnvelope(f, now, nil))
	f.Add(testEnvelope(f, now.In(time.FixedZone("x", 3600)), nil))
	f.Add(testEnvelope(f, now, func(e *accessEnvelope) { e.Time = 0 }))
	f.Add(testEnvelope(f, now, func(e *accessEnvelope) { e.MemoryKiB = 1 << 30 }))
	f.Add(testEnvelope(f, now, func(e *accessEnvelope) { e.Threads = 0 }))
	f.Add(testEnvelope(f, now, func(e *accessEnvelope) { e.Salt = make([]byte, 65) }))
	f.Add(testEnvelope(f, now, func(e *accessEnvelope) { e.Nonce = nil }))
	f.Add(testEnvelope(f, now, func(e *accessEnvelope) { e.Version = 2 }))
	f.Add([]byte(`{"format":"backupzit-access","version":1,"created":"9999-12-31T23:59:59.999999999+23:59","kdf":"argon2id"}`))
	f.Add([]byte(`{"format":"backupzit-access","version":1,"time":-1,"memory_kib":1e99,"threads":256}`))
	f.Add([]byte(`{"format":"backupzit-access","Format":"x","version":1.0,"salt":null}`))
	f.Add([]byte(`{"format":"backupzit-access"`))
	f.Add([]byte(`null`))
	f.Add([]byte{})
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	f.Fuzz(func(t *testing.T, b []byte) {
		e, err := parseEnvelope(b)
		if err != nil {
			if e != nil {
				t.Fatal("envelope and error")
			}
			return
		}
		if err := checkKDF(e); err != nil {
			t.Fatalf("accepted out of bounds: %v", err)
		}
		if len(e.Nonce) != accessNonceSize || len(e.Ciphertext) < accessTagSize || len(b) > MaxAccessPackageSize {
			t.Fatalf("accepted nonce %d, ciphertext %d, size %d", len(e.Nonce), len(e.Ciphertext), len(b))
		}
		// The header written again parses to the same additional data:
		// otherwise an honest package could not be opened.
		aad, err := e.aad()
		if err != nil {
			t.Fatal(err)
		}
		again, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		e2, err := parseEnvelope(again)
		if err != nil {
			t.Fatalf("re-marshalled envelope refused: %v", err)
		}
		if aad2, _ := e2.aad(); !bytes.Equal(aad, aad2) {
			t.Fatalf("additional data not stable:\n%s\n%s", aad, aad2)
		}
		a, err := testAEAD(key)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := openEnvelope(e, a); !errors.Is(err, ErrAccessPassphrase) && !errors.Is(err, ErrAccessDamaged) {
			t.Fatalf("random key opened the package: %v", err)
		}
	})
}

// testAEAD is AES-256-GCM with a key given directly, to decrypt without
// the cost of Argon2id.
func testAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// The salt and nonce in the clear are base64 in JSON; the parser must take
// what encoding/json writes for []byte, nothing else.
func TestEnvelopeSaltEncoding(t *testing.T) {
	salt := bytes.Repeat([]byte{0xfb}, 16)
	b := testEnvelope(t, time.Now(), func(e *accessEnvelope) { e.Salt = salt })
	if !bytes.Contains(b, []byte(base64.StdEncoding.EncodeToString(salt))) {
		t.Fatalf("salt not standard base64: %s", b)
	}
	e, err := parseEnvelope(b)
	if err != nil || !bytes.Equal(e.Salt, salt) {
		t.Fatalf("parse: %v", err)
	}
}
