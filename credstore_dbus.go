//go:build (dragonfly && cgo) || (freebsd && cgo) || linux || netbsd || openbsd

// The systems where go-keyring uses the Secret Service on D-Bus (its keyring_unix.go).

package main

import (
	"errors"
	"slices"
	"strings"

	"github.com/godbus/dbus/v5"
)

const secretService = "org.freedesktop.secrets"

// secretServiceMissing is true when a keyring error comes from there being no Secret Service: no D-Bus
// session at all, or one where nothing provides or can start org.freedesktop.secrets. Anything else (a
// locked or refusing keyring) is a real error.
func secretServiceMissing(err error) bool {
	var de dbus.Error
	var dp *dbus.Error
	name := ""
	switch {
	case errors.As(err, &de):
		name = de.Name
	case errors.As(err, &dp):
		name = dp.Name
	}
	if name == "org.freedesktop.DBus.Error.ServiceUnknown" || strings.HasPrefix(name, "org.freedesktop.DBus.Error.Spawn.") {
		return true // nothing provides it, or it failed to start (a keyring daemon without a display)
	}
	conn, cerr := dbus.SessionBus()
	if cerr != nil {
		return true // no session bus: no Secret Service either
	}
	bus := conn.BusObject()
	var owned bool
	var names []string
	if bus.Call("org.freedesktop.DBus.NameHasOwner", 0, secretService).Store(&owned) != nil ||
		bus.Call("org.freedesktop.DBus.ListActivatableNames", 0).Store(&names) != nil {
		return false // can't tell: report the keyring's error
	}
	return !owned && !slices.Contains(names, secretService)
}
