//go:build !(darwin || linux || freebsd || openbsd || netbsd || dragonfly)

package main

import "os"

// tryLock: line-lx isn't released for this OS, and it has no flock: syncs aren't kept apart.
func tryLock(*os.File) (bool, error) { return true, nil }
