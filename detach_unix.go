//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package main

import (
	"os/exec"
	"syscall"
)

// detach starts the command in a session of its own: a signal to the parent's process group (Claude Code stopping
// a hook) doesn't reach it.
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
