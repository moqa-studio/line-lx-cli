package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// lockWait is how long a sync waits for another line-lx on this machine to finish its own.
var lockWait = 30 * time.Second

// syncLock lets one line-lx at a time write to this machine's agents. Two syncs would share the staging
// folders (<config dir>/line-lx/claude-code/.staging, ~/.codex/.line-lx-staging, ~/.pi/agent/.line-lx-staging)
// and each clears them when it starts, under the other's feet. The lock is per person, not per hub: the
// agents' folders are too. It is a lock on <config dir>/line-lx/sync.lock, which the OS drops when line-lx
// exits, however it exits, so it never goes stale.
func syncLock() (unlock func(), err error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	dir = filepath.Join(dir, "line-lx")
	path := filepath.Join(dir, "sync.lock")
	if err := realDir(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s isn't a plain file (a symlink?), so line-lx won't use it", path)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("Can't open %s: %v", path, err)
	}
	deadline, waiting := time.Now().Add(lockWait), false
	for {
		locked, err := tryLock(f)
		switch {
		case err != nil:
			f.Close()
			return nil, fmt.Errorf("Can't lock %s: %v", path, err)
		case locked:
			return func() { f.Close() }, nil
		case time.Now().After(deadline):
			f.Close()
			return nil, errors.New("Another line-lx is syncing this machine. Run this again once it's done.")
		case !waiting:
			fmt.Println("Waiting for another line-lx on this machine to finish its sync…")
			waiting = true
		}
		time.Sleep(200 * time.Millisecond)
	}
}
