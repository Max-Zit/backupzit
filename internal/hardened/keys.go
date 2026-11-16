package hardened

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

// Keys are the access keys clients authenticate with. The file holds one
// "<name> <sha256 of key> [option=value ...]" per line; keys themselves are
// shown only once. Options are the key's Limits.
type Keys struct {
	path string

	mu   sync.Mutex
	sig  string
	list []keyEntry
}

// Limits restrict what one access key may do. Zero values mean no limit.
type Limits struct {
	// Path binds the key to one repository path: names outside it do not
	// exist for the key (404).
	Path string
	// Quota is the most bytes stored below Path (deleted files that are
	// still retained count, as they still use disk space). Without Path it
	// applies to the whole storage.
	Quota int64
	// Daily is the most new bytes the key may upload per day (UTC).
	Daily int64
	// Rate is the most write requests (upload, delete, keep) per minute.
	Rate int
}

func (l Limits) String() string {
	var s []string
	if l.Path != "" {
		s = append(s, "path="+l.Path)
	}
	if l.Quota > 0 {
		s = append(s, "quota="+strconv.FormatInt(l.Quota, 10))
	}
	if l.Daily > 0 {
		s = append(s, "daily="+strconv.FormatInt(l.Daily, 10))
	}
	if l.Rate > 0 {
		s = append(s, "rate="+strconv.Itoa(l.Rate))
	}
	return strings.Join(s, " ")
}

// Allows reports whether name lies within the key's path.
func (l Limits) Allows(name string) bool {
	return l.Path == "" || name == l.Path || strings.HasPrefix(name, l.Path+"/")
}

// Check validates the limits, e.g. before they are saved.
func (l Limits) Check() error {
	if l.Path != "" && (!ValidName(l.Path) || isLockFile(l.Path+"/x")) {
		return fmt.Errorf("invalid path %q: letters, digits, '.', '_', '-' and '/' between names", l.Path)
	}
	if l.Quota < 0 || l.Daily < 0 || l.Rate < 0 {
		return errors.New("limits cannot be negative")
	}
	return nil
}

// Key is an authenticated access key.
type Key struct {
	Name string
	Limits
}

type keyEntry struct {
	name string
	hash []byte
	lim  Limits
}

func NewKeys(path string) *Keys { return &Keys{path: path} }

func (k *Keys) signature() string {
	fi, err := os.Stat(k.path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d/%d", fi.Size(), fi.ModTime().UnixNano())
}

// parseKeyLine reads one line of the key file; ok is false for comments
// and malformed lines. An unknown or invalid option makes the line invalid,
// so a mistyped limit never turns into "no limit".
func parseKeyLine(line string) (keyEntry, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 || strings.HasPrefix(fields[0], "#") {
		return keyEntry{}, false
	}
	h, err := hex.DecodeString(fields[1])
	if err != nil || len(h) != sha256.Size {
		return keyEntry{}, false
	}
	e := keyEntry{name: fields[0], hash: h}
	for _, opt := range fields[2:] {
		k, v, ok := strings.Cut(opt, "=")
		if !ok {
			return keyEntry{}, false
		}
		var err error
		switch k {
		case "path":
			e.lim.Path = v
		case "quota":
			e.lim.Quota, err = strconv.ParseInt(v, 10, 64)
		case "daily":
			e.lim.Daily, err = strconv.ParseInt(v, 10, 64)
		case "rate":
			e.lim.Rate, err = strconv.Atoi(v)
		default:
			return keyEntry{}, false
		}
		if err != nil {
			return keyEntry{}, false
		}
	}
	if e.lim.Check() != nil {
		return keyEntry{}, false
	}
	return e, true
}

func (e keyEntry) line() string {
	s := e.name + " " + hex.EncodeToString(e.hash)
	if o := e.lim.String(); o != "" {
		s += " " + o
	}
	return s
}

// load re-reads the file when it changed, so keys added with the CLI work
// without restarting the service.
func (k *Keys) load() ([]keyEntry, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	sig := k.signature()
	if sig == k.sig && k.list != nil {
		return k.list, nil
	}
	f, err := os.Open(k.path)
	if errors.Is(err, os.ErrNotExist) {
		k.list, k.sig = []keyEntry{}, sig
		return k.list, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var list []keyEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if e, ok := parseKeyLine(sc.Text()); ok {
			list = append(list, e)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	k.list, k.sig = list, sig
	return list, nil
}

// Check returns the name of the key matching token.
func (k *Keys) Check(token string) (string, bool) {
	key, ok := k.Lookup(token)
	return key.Name, ok
}

// Lookup returns the key matching token with its limits.
func (k *Keys) Lookup(token string) (Key, bool) {
	list, err := k.load()
	if err != nil || token == "" {
		return Key{}, false
	}
	h := sha256.Sum256([]byte(token))
	var key Key
	ok := false
	for _, e := range list {
		if subtle.ConstantTimeCompare(e.hash, h[:]) == 1 {
			key, ok = Key{Name: e.name, Limits: e.lim}, true
		}
	}
	return key, ok
}

// Names lists the key names.
func (k *Keys) Names() ([]string, error) {
	list, err := k.load()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range list {
		out = append(out, e.name)
	}
	return out, nil
}

// List returns the keys (without secrets) with their limits.
func (k *Keys) List() ([]Key, error) {
	list, err := k.load()
	if err != nil {
		return nil, err
	}
	out := make([]Key, 0, len(list))
	for _, e := range list {
		out = append(out, Key{Name: e.name, Limits: e.lim})
	}
	return out, nil
}

// Add creates a key without limits and returns it; it cannot be shown again.
func (k *Keys) Add(name string) (string, error) { return k.AddWithLimits(name, Limits{}) }

// AddWithLimits creates a key with limits and returns it; it cannot be
// shown again.
func (k *Keys) AddWithLimits(name string, lim Limits) (string, error) {
	if !ValidName(name) || strings.Contains(name, "/") {
		return "", errors.New("key name may contain only letters, digits, '.', '_' and '-'")
	}
	if err := lim.Check(); err != nil {
		return "", err
	}
	names, err := k.Names()
	if err != nil {
		return "", err
	}
	for _, n := range names {
		if n == name {
			return "", fmt.Errorf("key %q already exists", name)
		}
	}
	b := make([]byte, 25)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := "bzr_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
	h := sha256.Sum256([]byte(token))
	f, err := os.OpenFile(k.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := fmt.Fprintln(f, keyEntry{name: name, hash: h[:], lim: lim}.line()); err != nil {
		return "", err
	}
	return token, nil
}

// SetLimits replaces the limits of the named key; the key stays the same.
func (k *Keys) SetLimits(name string, lim Limits) error {
	if err := lim.Check(); err != nil {
		return err
	}
	return k.rewrite(name, func(e keyEntry) (keyEntry, bool) {
		e.lim = lim
		return e, true
	})
}

// Remove deletes the named key.
func (k *Keys) Remove(name string) error {
	return k.rewrite(name, func(e keyEntry) (keyEntry, bool) { return e, false })
}

// rewrite applies f to the line of the named key (dropping it when f
// returns false) and keeps every other line as it is.
func (k *Keys) rewrite(name string, f func(keyEntry) (keyEntry, bool)) error {
	b, err := os.ReadFile(k.path)
	if err != nil {
		return err
	}
	var out []string
	found := false
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if e, ok := parseKeyLine(line); ok && e.name == name {
			found = true
			if e, keep := f(e); keep {
				out = append(out, e.line())
			}
			continue
		}
		if fl := strings.Fields(line); len(fl) >= 2 && fl[0] == name {
			// An unreadable line of this key (e.g. a mistyped limit,
			// which disables the key) can be removed, not changed.
			if _, keep := f(keyEntry{name: name}); keep {
				return fmt.Errorf("the line of key %q in %s is not valid; correct or remove it", name, k.path)
			}
			found = true
			continue
		}
		out = append(out, line)
	}
	if !found {
		return fmt.Errorf("no key named %q", name)
	}
	content := strings.Join(out, "\n")
	if content != "" {
		content += "\n"
	}
	return os.WriteFile(k.path, []byte(content), 0o600) // #nosec G703 -- key file of the repository server's own configuration
}
