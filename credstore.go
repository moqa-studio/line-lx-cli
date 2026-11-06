package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/zalando/go-keyring"
)

// --- Where sign-ins live: the OS keychain, else a file only the person can read ---
//
// macOS always has a keychain. Linux has one only through a Secret Service on D-Bus
// (GNOME Keyring, KWallet), which SSH boxes, containers and WSL usually lack. There, and only there,
// line-lx keeps each sign-in in <config dir>/line-lx/credentials/<hub id>.json: mode 0600 in a 0700 folder,
// and it refuses to read one that is a symlink or that others can read. A keychain that is there but
// fails is an error, never a reason to write a file.

// noKeychain is set once this machine is found to have no keychain at all.
var noKeychain bool

var fileNotice sync.Once

// keychainMissing tells "this machine has no keychain" from a keychain that failed.
func keychainMissing(err error) bool {
	return errors.Is(err, keyring.ErrUnsupportedPlatform) || secretServiceMissing(err)
}

// credGet returns the sign-in stored for the hub; keyring.ErrNotFound when there's none.
func credGet(hub string) (string, error) {
	if !noKeychain {
		v, err := keyring.Get(keychainService, hub)
		if err == nil || errors.Is(err, keyring.ErrNotFound) {
			return v, err
		}
		if !keychainMissing(err) {
			return "", fmt.Errorf("Can't read the keychain: %v", err)
		}
		noKeychain = true
	}
	return fileGet(hub)
}

func credSet(hub, secret string) error {
	if !noKeychain {
		err := keyring.Set(keychainService, hub, secret)
		if err == nil {
			return nil
		}
		if !keychainMissing(err) {
			return fmt.Errorf("Can't save the sign-in to the keychain (%v). On macOS this is the login keychain of the current user.", err)
		}
		noKeychain = true
	}
	path, err := fileSet(hub, secret)
	if err != nil {
		return fmt.Errorf("Can't save the sign-in: %v", err)
	}
	fileNotice.Do(func() {
		fmt.Fprintf(os.Stderr, "No keychain on this machine (no Secret Service on D-Bus), so line-lx keeps the sign-in in %s, readable only by you.\n", path)
	})
	return nil
}

// credDelete forgets the hub's sign-in, in the keychain and in a file; keyring.ErrNotFound when neither
// had it.
func credDelete(hub string) error {
	found := false
	if !noKeychain {
		err := keyring.Delete(keychainService, hub)
		switch {
		case err == nil:
			found = true
		case errors.Is(err, keyring.ErrNotFound):
		case keychainMissing(err):
			noKeychain = true
		default:
			return fmt.Errorf("Can't remove the sign-in from the keychain: %v", err)
		}
	}
	path, err := credPath(hub)
	if err != nil {
		return err
	}
	switch err := os.Remove(path); {
	case err == nil:
		found = true
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("Can't remove %s: %v", path, err)
	}
	if !found {
		return keyring.ErrNotFound
	}
	return nil
}

func credPath(hub string) (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "line-lx", "credentials", hubID(hub)+".json"), nil
}

// credFile is a sign-in file: the hub it's for, and what the keychain would hold.
type credFile struct {
	Hub    string `json:"hub"`
	Secret string `json:"secret"`
}

func fileGet(hub string) (string, error) {
	path, err := credPath(hub)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", keyring.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("Can't read %s: %v", path, err)
	}
	if err := privateFile(path, info); err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("Can't read %s: %v", path, err)
	}
	defer f.Close()
	// The file opened must be the one checked: not swapped for a symlink or another file in between.
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return "", fmt.Errorf("%s changed while line-lx read it, so line-lx won't use it", path)
	}
	if err := privateFile(path, opened); err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return "", fmt.Errorf("Can't read %s: %v", path, err)
	}
	var cf credFile
	if json.Unmarshal(data, &cf) != nil || cf.Hub != hub {
		// Unreadable, or for another hub: loadCreds says to sign in again.
		return "", nil
	}
	return cf.Secret, nil
}

// privateFile refuses a sign-in file that is a symlink or not a plain file, or that others can read.
func privateFile(path string, info fs.FileInfo) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s isn't a plain file (a symlink?), so line-lx won't read it", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("Other users can read or change %s, so line-lx won't use it. Delete it, then run `line-lx login`", path)
	}
	return nil
}

// fileSet writes the sign-in file through a temp file (created 0600) and a rename, so it is never half
// written, nor readable by others for a moment. It returns the file's path.
func fileSet(hub, secret string) (string, error) {
	path, err := credPath(hub)
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(path)
	for _, d := range []string{filepath.Dir(dir), dir} {
		if err := realDir(d); err != nil {
			return "", err
		}
		if err := os.MkdirAll(d, 0o700); err != nil {
			return "", err
		}
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	data, _ := json.Marshal(credFile{Hub: hub, Secret: secret})
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	return path, err
}
