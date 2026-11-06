//go:build !((dragonfly && cgo) || (freebsd && cgo) || linux || netbsd || openbsd)

package main

// secretServiceMissing: macOS always has a keychain, so a keychain error is a real one.
func secretServiceMissing(error) bool { return false }
