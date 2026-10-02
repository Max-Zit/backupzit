package server

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Secrets stored by the console (storage passwords and keys, recovery keys,
// SMTP and LDAP passwords, two-factor secrets) are encrypted with
// AES-256-GCM under a key kept in a file next to the database, not in it.
// A database dump or backup alone therefore reveals no credentials.

const secretPrefix = "enc:v1:"

// SecretKeyFile is the key's file name in the data directory.
const SecretKeyFile = "secrets.key"

type secretBox struct {
	aead cipher.AEAD
	id   string // short fingerprint shown in Settings
}

func newSecretBox(key []byte) (*secretBox, error) {
	if len(key) != 32 {
		return nil, errors.New("secrets key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	h := sha256.Sum256(append([]byte("backupzit-secrets-id:"), key...))
	return &secretBox{aead: aead, id: base64.RawURLEncoding.EncodeToString(h[:6])}, nil
}

// seal encrypts a value; context binds it to its field so ciphertexts
// cannot be swapped between fields.
func (b *secretBox) seal(context, plain string) string {
	if b == nil || plain == "" || strings.HasPrefix(plain, secretPrefix) {
		return plain
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	ct := b.aead.Seal(nonce, nonce, []byte(plain), []byte(context))
	return secretPrefix + base64.RawStdEncoding.EncodeToString(ct)
}

// open decrypts a value; values without the prefix are legacy plaintext.
func (b *secretBox) open(context, stored string) (string, error) {
	if !strings.HasPrefix(stored, secretPrefix) {
		return stored, nil
	}
	if b == nil {
		return "", errors.New("encrypted secret but no secrets key is loaded")
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(stored, secretPrefix))
	if err != nil || len(raw) < b.aead.NonceSize() {
		return "", errors.New("corrupt encrypted secret")
	}
	n := b.aead.NonceSize()
	pt, err := b.aead.Open(nil, raw[:n], raw[n:], []byte(context))
	if err != nil {
		return "", fmt.Errorf("cannot decrypt %s: wrong secrets key?", context)
	}
	return string(pt), nil
}

// LoadOrCreateSecretKey reads <dir>/secrets.key, creating a random key on
// first start. BACKUPZIT_SECRETS_KEY (base64) overrides the file.
func LoadOrCreateSecretKey(dir string) ([]byte, bool, error) {
	if v := os.Getenv("BACKUPZIT_SECRETS_KEY"); v != "" {
		k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v))
		if err != nil || len(k) != 32 {
			return nil, false, errors.New("BACKUPZIT_SECRETS_KEY must be 32 bytes, base64 encoded")
		}
		return k, false, nil
	}
	p := filepath.Join(dir, SecretKeyFile)
	b, err := os.ReadFile(p)
	if err == nil {
		k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(k) != 32 {
			return nil, false, fmt.Errorf("%s is not a valid secrets key", p)
		}
		return k, false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, false, err
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, false, err
	}
	if err := os.WriteFile(p, []byte(base64.StdEncoding.EncodeToString(k)+"\n"), 0o600); err != nil {
		return nil, false, err
	}
	return k, true, nil
}

// UseSecretKey enables encryption of stored secrets.
func (s *Store) UseSecretKey(key []byte) error {
	b, err := newSecretBox(key)
	if err != nil {
		return err
	}
	s.box = b
	return nil
}

// SecretKeyID identifies the loaded key (empty when none is loaded).
func (s *Store) SecretKeyID() string {
	if s.box == nil {
		return ""
	}
	return s.box.id
}

// secretSettings are settings that contain passwords.
var secretSettings = map[string]bool{settingEmail: true, settingLDAP: true}

// target columns holding secrets, in scan order of encryptTarget.
const ctxTarget = "storage_targets."

func (s *Store) encryptTarget(t *Target) {
	t.SFTPPassword = s.box.seal(ctxTarget+"sftp_password", t.SFTPPassword)
	t.SFTPKey = s.box.seal(ctxTarget+"sftp_key", t.SFTPKey)
	t.S3SecretKey = s.box.seal(ctxTarget+"s3_secret_key", t.S3SecretKey)
	t.SMBPassword = s.box.seal(ctxTarget+"smb_password", t.SMBPassword)
	t.HardenedKey = s.box.seal(ctxTarget+"hardened_key", t.HardenedKey)
	t.RecoveryKey = s.box.seal(ctxTarget+"repo_password", t.RecoveryKey)
	t.AzureKey = s.box.seal(ctxTarget+"azure_key", t.AzureKey)
	t.AzureSAS = s.box.seal(ctxTarget+"azure_sas", t.AzureSAS)
}

func (s *Store) decryptTarget(t *Target) error {
	var err error
	for _, f := range []struct {
		col string
		v   *string
	}{
		{"sftp_password", &t.SFTPPassword}, {"sftp_key", &t.SFTPKey}, {"s3_secret_key", &t.S3SecretKey},
		{"smb_password", &t.SMBPassword}, {"hardened_key", &t.HardenedKey}, {"repo_password", &t.RecoveryKey},
		{"azure_key", &t.AzureKey}, {"azure_sas", &t.AzureSAS},
	} {
		if *f.v, err = s.box.open(ctxTarget+f.col, *f.v); err != nil {
			return err
		}
	}
	return nil
}

// sealSetting wraps a JSON setting value as an encrypted JSON string.
func (s *Store) sealSetting(key string, b []byte) ([]byte, error) {
	if s.box == nil || !secretSettings[key] {
		return b, nil
	}
	return json.Marshal(s.box.seal("settings."+key, string(b)))
}

func (s *Store) openSetting(key string, b []byte) ([]byte, error) {
	if !strings.HasPrefix(string(b), `"`+secretPrefix) {
		return b, nil
	}
	var str string
	if err := json.Unmarshal(b, &str); err != nil {
		return nil, err
	}
	pt, err := s.box.open("settings."+key, str)
	return []byte(pt), err
}

// EncryptExistingSecrets encrypts secrets stored in plaintext by earlier
// versions. It is idempotent and runs at every start.
func (s *Store) EncryptExistingSecrets(ctx context.Context) (int, error) {
	if s.box == nil {
		return 0, nil
	}
	n := 0
	targets, err := s.ListTargets(ctx)
	if err != nil {
		return 0, err
	}
	for _, t := range targets {
		var raw [6]string
		err := s.db.QueryRow(ctx, `SELECT sftp_password, sftp_key, s3_secret_key, smb_password, hardened_key, repo_password
			FROM storage_targets WHERE id=$1`, t.ID).Scan(&raw[0], &raw[1], &raw[2], &raw[3], &raw[4], &raw[5])
		if err != nil {
			return n, err
		}
		plain := false
		for _, v := range raw {
			if v != "" && !strings.HasPrefix(v, secretPrefix) {
				plain = true
			}
		}
		if !plain {
			continue
		}
		s.encryptTarget(&t)
		if _, err := s.db.Exec(ctx, `UPDATE storage_targets SET sftp_password=$2, sftp_key=$3, s3_secret_key=$4,
			smb_password=$5, hardened_key=$6, repo_password=$7 WHERE id=$1`,
			t.ID, t.SFTPPassword, t.SFTPKey, t.S3SecretKey, t.SMBPassword, t.HardenedKey, t.RecoveryKey); err != nil {
			return n, err
		}
		n++
	}
	for key := range secretSettings {
		var b []byte
		if err := s.db.QueryRow(ctx, `SELECT value FROM settings WHERE key=$1`, key).Scan(&b); err != nil {
			continue
		}
		if strings.HasPrefix(string(b), `"`+secretPrefix) {
			continue
		}
		sealed, err := s.sealSetting(key, b)
		if err != nil {
			return n, err
		}
		if _, err := s.db.Exec(ctx, `UPDATE settings SET value=$2 WHERE key=$1`, key, sealed); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
