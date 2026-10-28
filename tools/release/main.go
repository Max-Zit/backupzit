// Command release creates the release signing key and signs release
// manifests for the console's self-update.
//
//	release keygen -out DIR                  # once; keep DIR/release.key secret
//	release sign -key KEY -version X -dir dist [-notes FILE]
//	release notes -key KEY -dir DIR -notes FILE  # new notes, signed again
//
// sign writes DIR/manifest.json and manifest.json.sig listing the server,
// agent and repository packages of version X found in the directory.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/update"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: release keygen|sign|notes [options]")
	}
	switch args[0] {
	case "keygen":
		fs := flag.NewFlagSet("keygen", flag.ExitOnError)
		out := fs.String("out", ".", "directory for release.key and release.pub")
		fs.Parse(args[1:])
		p := filepath.Join(*out, "release.key")
		if _, err := os.Stat(p); err == nil {
			return fmt.Errorf("%s exists; refusing to overwrite the release key", p)
		}
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(*out, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(base64.StdEncoding.EncodeToString(priv.Seed())+"\n"), 0o600); err != nil {
			return err
		}
		pubB64 := base64.StdEncoding.EncodeToString(pub)
		if err := os.WriteFile(filepath.Join(*out, "release.pub"), []byte(pubB64+"\n"), 0o644); err != nil {
			return err
		}
		fmt.Println("public key (add to update.TrustedKeys):", pubB64)
		return nil
	case "notes":
		// Replaces the notes of a signed manifest (DIR/manifest.json) and
		// signs it again; the packages and their checksums stay.
		fs := flag.NewFlagSet("notes", flag.ExitOnError)
		keyPath := fs.String("key", "", "release.key")
		dir := fs.String("dir", "", "directory with manifest.json")
		notes := fs.String("notes", "", "file with the new release notes")
		fs.Parse(args[1:])
		if *keyPath == "" || *dir == "" || *notes == "" {
			return errors.New("-key, -dir and -notes are required")
		}
		key, err := loadKey(*keyPath)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(filepath.Join(*dir, "manifest.json"))
		if err != nil {
			return err
		}
		var m update.Manifest
		if err := json.Unmarshal(raw, &m); err != nil {
			return fmt.Errorf("manifest.json: %w", err)
		}
		if m.Product != "backupzit" || m.Version == "" || len(m.Files) == 0 {
			return errors.New("manifest.json is not a BackupZit release manifest")
		}
		n, err := os.ReadFile(*notes)
		if err != nil {
			return err
		}
		m.Notes = strings.TrimSpace(string(n))
		if err := writeManifest(*dir, m, key); err != nil {
			return err
		}
		fmt.Printf("signed manifest for %s again with new notes\n", m.Version)
		return nil
	case "sign":
		fs := flag.NewFlagSet("sign", flag.ExitOnError)
		keyPath := fs.String("key", "", "release.key")
		version := fs.String("version", "", "release version, e.g. 0.28.0")
		dir := fs.String("dir", "dist", "directory with the packages; manifest is written there")
		notes := fs.String("notes", "", "file with release notes")
		fs.Parse(args[1:])
		if *keyPath == "" || *version == "" {
			return errors.New("-key and -version are required")
		}
		key, err := loadKey(*keyPath)
		if err != nil {
			return err
		}
		m := update.Manifest{Product: "backupzit", Version: *version, Released: time.Now().UTC().Truncate(time.Second)}
		if *notes != "" {
			n, err := os.ReadFile(*notes)
			if err != nil {
				return err
			}
			m.Notes = strings.TrimSpace(string(n))
		}
		entries, err := os.ReadDir(*dir)
		if err != nil {
			return err
		}
		v := *version
		for _, e := range entries {
			name := e.Name()
			f := update.File{Name: name}
			switch {
			case name == "backupzit-server_"+v+"_amd64.deb":
				f.Kind, f.Format, f.Arch = "server", "deb", "amd64"
			case name == "backupzit-server-"+v+"-1.x86_64.rpm":
				f.Kind, f.Format, f.Arch = "server", "rpm", "amd64"
			case name == "backupzit-agent_"+v+"_amd64.deb":
				f.Kind, f.Format, f.Arch = "agent", "deb", "amd64"
			case name == "backupzit-agent-"+v+"-1.x86_64.rpm":
				f.Kind, f.Format, f.Arch = "agent", "rpm", "amd64"
			case name == "backupzit-agent-"+v+"-x64.msi":
				f.Kind, f.Format, f.Arch = "agent", "msi", "amd64"
			case name == "backupzit-agent-"+v+"-x64-legacy.msi":
				f.Kind, f.Format, f.Arch = "agent-legacy", "msi", "amd64"
			case name == "backupzit-repo_"+v+"_amd64.deb":
				f.Kind, f.Format, f.Arch = "repo", "deb", "amd64"
			case name == "backupzit-repo-"+v+"-1.x86_64.rpm":
				f.Kind, f.Format, f.Arch = "repo", "rpm", "amd64"
			default:
				continue
			}
			fh, err := os.Open(filepath.Join(*dir, name))
			if err != nil {
				return err
			}
			h := sha256.New()
			f.Size, err = io.Copy(h, fh)
			fh.Close()
			if err != nil {
				return err
			}
			f.SHA256 = hex.EncodeToString(h.Sum(nil))
			m.Files = append(m.Files, f)
		}
		if len(m.Files) == 0 {
			return fmt.Errorf("no packages of version %s in %s", v, *dir)
		}
		if err := writeManifest(*dir, m, key); err != nil {
			return err
		}
		fmt.Printf("signed manifest for %s with %d packages\n", v, len(m.Files))
		return nil
	}
	return fmt.Errorf("unknown command %q", args[0])
}

func loadKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("invalid release key")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// writeManifest writes DIR/manifest.json and its signature.
func writeManifest(dir string, m update.Manifest, key ed25519.PrivateKey) error {
	raw, _ := json.MarshalIndent(m, "", "  ")
	raw = append(raw, '\n')
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "manifest.json.sig"), []byte(update.Sign(raw, key)+"\n"), 0o644)
}
