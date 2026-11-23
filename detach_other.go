//go:build !(darwin || linux || freebsd || openbsd || netbsd || dragonfly)

package main

import "os/exec"

// detach: line-lx isn't released for this OS; the command starts as it is.
func detach(*exec.Cmd) {}
