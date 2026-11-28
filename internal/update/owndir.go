package update

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
)

// The root helper works in Dir (/var/lib/backupzit/update), a folder the
// console's user owns, as it owns the folder above: that user could rename
// Dir away and put a link to /etc in its place, and root would then read
// the request from, write its results into and remove files in the folder
// the link points to (L-N23). So Dir must be a real folder (not a link) of
// root or of the console's user that nobody else may write to, and every
// read, write, rename and removal in it goes through one handle of that
// folder (os.Root): a link inside cannot lead out of it, and a swap of Dir
// after the check changes nothing.

// openOwnDir opens dir as an os.Root after checking it.
func openOwnDir(dir string) (*os.Root, error) {
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a folder (a link?); the update helper does not use it", dir)
	}
	if err := checkDirOwner(dir, fi); err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	// The folder opened must be the one checked (OpenRoot follows a link).
	opened, err := r.Stat(".")
	if err != nil || !os.SameFile(fi, opened) {
		_ = r.Close()
		return nil, fmt.Errorf("%s changed while it was opened; the update helper does not use it", dir)
	}
	return r, nil
}

// writeJSONIn is WriteJSON inside the folder r: a new file with a random
// name (never through a link) renamed over name.
func writeJSONIn(r *os.Root, name string, v any, mode os.FileMode) error {
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	tmp := "." + name + "." + hex.EncodeToString(rnd[:]) + ".tmp"
	f, err := r.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(marshal(v))
	if err == nil {
		err = f.Chmod(mode)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = r.Rename(tmp, name)
	}
	if err != nil {
		_ = r.Remove(tmp)
	}
	return err
}

// readJSONIn is ReadJSON for a file of the folder r.
func readJSONIn(r *os.Root, name string, v any) error {
	b, err := r.ReadFile(name)
	if err != nil {
		return err
	}
	return json.NewDecoder(bytes.NewReader(b)).Decode(v)
}

// copyRegularIn is copyRegular for a file of the folder r.
func copyRegularIn(r *os.Root, name, dst string) error {
	fi, err := r.Lstat(name)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", name)
	}
	in, err := r.Open(name)
	if err != nil {
		return err
	}
	defer in.Close()
	return copyOpened(in, fi, name, dst)
}
