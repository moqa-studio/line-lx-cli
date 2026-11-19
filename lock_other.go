//go:build !(darwin || linux || freebsd || openbsd || netbsd || dragonfly)

package main

import "os"

// openNoFollow: this OS has no O_NOFOLLOW; the Lstat check before the open stays.
const openNoFollow = 0

// tryLock: line-lx isn't released for this OS, and it has no flock: syncs aren't kept apart.
func tryLock(*os.File) (bool, error) { return true, nil }
