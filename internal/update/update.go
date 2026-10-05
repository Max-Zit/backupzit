// Package update describes signed BackupZit releases. A release is a
// manifest (JSON) listing the packages with their SHA-256, signed with the
// project's Ed25519 release key. The console downloads and checks it; the
// root helper that installs a package checks it again with the keys built
// into the program, so a compromised console cannot install anything else.
package update

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/netcfg"
)

// TrustedKeys are the public release keys (base64). A release must be
// signed by one of them.
var TrustedKeys = []string{
	"+ad/LUTVtGXwVv4qyca7QgmSErAbXTFmje1iWyFlmGo=", // 2026-10-04, release.key kept offline by the maintainer
}

// Manifest describes one release.
type Manifest struct {
	Product  string    `json:"product"` // "backupzit"
	Version  string    `json:"version"`
	Released time.Time `json:"released"`
	Notes    string    `json:"notes,omitempty"`
	Files    []File    `json:"files"`
}

// File is a package of a release.
type File struct {
	Name   string `json:"name"`   // e.g. backupzit-server_0.28.0_amd64.deb
	Kind   string `json:"kind"`   // server, agent, repo
	Format string `json:"format"` // deb, rpm, msi
	Arch   string `json:"arch,omitempty"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Sign signs manifest bytes and returns the base64 signature.
func Sign(manifest []byte, key ed25519.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(key, manifest))
}

// Verify checks the signature of manifest bytes against keys (base64
// public keys; TrustedKeys when nil) and parses the manifest.
func Verify(manifest []byte, sig string, keys []string) (*Manifest, error) {
	if keys == nil {
		keys = TrustedKeys
	}
	s, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sig))
	if err != nil || len(s) != ed25519.SignatureSize {
		return nil, errors.New("invalid release signature")
	}
	ok := false
	for _, k := range keys {
		pub, err := base64.StdEncoding.DecodeString(k)
		if err == nil && len(pub) == ed25519.PublicKeySize && ed25519.Verify(ed25519.PublicKey(pub), manifest, s) {
			ok = true
			break
		}
	}
	if !ok {
		return nil, errors.New("the release is not signed with a BackupZit release key")
	}
	var m Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		return nil, fmt.Errorf("release manifest: %w", err)
	}
	if m.Product != "backupzit" || m.Version == "" {
		return nil, errors.New("not a BackupZit release manifest")
	}
	for _, f := range m.Files {
		if f.Name == "" || strings.ContainsAny(f.Name, `/\`) || f.Name == "." || f.Name == ".." || len(f.SHA256) != 64 {
			return nil, fmt.Errorf("invalid file entry %q in the manifest", f.Name)
		}
	}
	return &m, nil
}

// Find returns the file of the given kind and format.
func (m *Manifest) Find(kind, format, arch string) (File, bool) {
	for _, f := range m.Files {
		if f.Kind == kind && f.Format == format && (arch == "" || f.Arch == "" || f.Arch == arch) {
			return f, true
		}
	}
	return File{}, false
}

// CheckFile verifies a downloaded file against its manifest entry.
func CheckFile(p string, f File) error {
	fh, err := os.Open(p)
	if err != nil {
		return err
	}
	defer fh.Close()
	h := sha256.New()
	n, err := io.Copy(h, fh)
	if err != nil {
		return err
	}
	if f.Size > 0 && n != f.Size {
		return fmt.Errorf("%s has %d bytes, the release says %d", f.Name, n, f.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, f.SHA256) {
		return fmt.Errorf("%s is damaged or was changed (checksum mismatch)", f.Name)
	}
	return nil
}

// Source fetches release files from an HTTPS address or a local folder
// (file:// or a plain path), for networks without internet access.
type Source struct {
	Base string
	HTTP *http.Client
}

func (s Source) resolve(name string) (string, bool, error) {
	b := strings.TrimSpace(s.Base)
	if b == "" {
		return "", false, errors.New("no update source configured")
	}
	if strings.HasPrefix(b, "https://") || strings.HasPrefix(b, "http://") {
		u, err := url.Parse(b)
		if err != nil {
			return "", false, err
		}
		u.Path = path.Join(u.Path, name)
		return u.String(), true, nil
	}
	b = strings.TrimPrefix(b, "file://")
	return filepath.Join(b, name), false, nil
}

// Open returns a reader for a file of the source.
func (s Source) Open(ctx context.Context, name string) (io.ReadCloser, error) {
	p, remote, err := s.resolve(name)
	if err != nil {
		return nil, err
	}
	if !remote {
		return os.Open(p)
	}
	hc := s.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s: %s", p, resp.Status)
	}
	return resp.Body, nil
}

// Latest reads and verifies the source's manifest.json and its signature.
func (s Source) Latest(ctx context.Context) (*Manifest, []byte, string, error) {
	read := func(name string, max int64) ([]byte, error) {
		rc, err := s.Open(ctx, name)
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(io.LimitReader(rc, max))
	}
	raw, err := read("manifest.json", 1<<20)
	if err != nil {
		return nil, nil, "", err
	}
	sig, err := read("manifest.json.sig", 4096)
	if err != nil {
		return nil, nil, "", err
	}
	m, err := Verify(raw, string(sig), nil)
	if err != nil {
		return nil, nil, "", err
	}
	return m, raw, strings.TrimSpace(string(sig)), nil
}

// Download copies a release file to dst and verifies it.
func (s Source) Download(ctx context.Context, f File, dst string) error {
	rc, err := s.Open(ctx, f.Name)
	if err != nil {
		return err
	}
	defer rc.Close()
	tmp := dst + ".part"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := CheckFile(tmp, f); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// Newer reports whether version a is newer than b ("0.28.0" > "0.27.3";
// a suffix like "-legacy" is ignored).
func Newer(a, b string) bool {
	pa, pb := parseVersion(a), parseVersion(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func parseVersion(v string) [3]int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+ "); i >= 0 {
		v = v[:i]
	}
	var out [3]int
	for i, p := range strings.SplitN(v, ".", 3) {
		out[i], _ = strconv.Atoi(p)
	}
	return out
}

// Request asks the root helper to install a downloaded package, or to do
// an operating system task (Action).
type Request struct {
	Action    string            `json:"action,omitempty"`
	Enable    bool              `json:"enable,omitempty"`  // ActionOSAuto
	Network   *netcfg.Config    `json:"network,omitempty"` // ActionNetwork
	SSH       *netcfg.SSHConfig `json:"ssh,omitempty"`     // ActionSSH
	Manifest  []byte            `json:"manifest"`
	Signature string            `json:"signature"`
	File      string            `json:"file"` // name in the manifest; the package lies next to the request
	Requested time.Time         `json:"requested"`
	User      string            `json:"user"`
}

// Result is what the helper reports back.
type Result struct {
	Version  string    `json:"version"`
	From     string    `json:"from"`
	Status   string    `json:"status"` // installed, rolled-back, failed
	Message  string    `json:"message"`
	Finished time.Time `json:"finished"`
	DBBackup string    `json:"db_backup,omitempty"`
}

// marshal is json.Marshal with a trailing newline.
func marshal(v any) []byte {
	b, _ := json.MarshalIndent(v, "", "  ")
	return append(b, '\n')
}

// WriteJSON writes v atomically.
func WriteJSON(p string, v any, mode os.FileMode) error {
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, marshal(v), mode); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// ReadJSON reads a JSON file into v.
func ReadJSON(p string, v any) error {
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	return json.NewDecoder(bytes.NewReader(b)).Decode(v)
}
