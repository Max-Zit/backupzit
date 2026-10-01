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
	"strings"
	"sync"
)

// Keys are the access keys clients authenticate with. The file holds one
// "<name> <sha256 of key>" per line; keys themselves are shown only once.
type Keys struct {
	path string

	mu   sync.Mutex
	sig  string
	list []keyEntry
}

type keyEntry struct {
	name string
	hash []byte
}

func NewKeys(path string) *Keys { return &Keys{path: path} }

func (k *Keys) signature() string {
	fi, err := os.Stat(k.path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d/%d", fi.Size(), fi.ModTime().UnixNano())
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
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		h, err := hex.DecodeString(fields[1])
		if err != nil || len(h) != sha256.Size {
			continue
		}
		list = append(list, keyEntry{name: fields[0], hash: h})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	k.list, k.sig = list, sig
	return list, nil
}

// Check returns the name of the key matching token.
func (k *Keys) Check(token string) (string, bool) {
	list, err := k.load()
	if err != nil || token == "" {
		return "", false
	}
	h := sha256.Sum256([]byte(token))
	name, ok := "", false
	for _, e := range list {
		if subtle.ConstantTimeCompare(e.hash, h[:]) == 1 {
			name, ok = e.name, true
		}
	}
	return name, ok
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

// Add creates a key and returns it; it cannot be shown again.
func (k *Keys) Add(name string) (string, error) {
	if !ValidName(name) || strings.Contains(name, "/") {
		return "", errors.New("key name may contain only letters, digits, '.', '_' and '-'")
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
	if _, err := fmt.Fprintf(f, "%s %s\n", name, hex.EncodeToString(h[:])); err != nil {
		return "", err
	}
	return token, nil
}

// Remove deletes the named key.
func (k *Keys) Remove(name string) error {
	b, err := os.ReadFile(k.path)
	if err != nil {
		return err
	}
	var out []string
	found := false
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == name {
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
	return os.WriteFile(k.path, []byte(content), 0o600)
}
