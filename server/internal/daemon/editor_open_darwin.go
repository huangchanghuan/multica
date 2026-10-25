//go:build darwin

package daemon

import "syscall"

// editorSpawnSysProcAttr detaches the editor from the daemon's process group.
// The daemon runs inside the user's Aqua session (LaunchAgent), so the child
// inherits a working WindowServer connection and needs no extra environment.
func editorSpawnSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// graphicalEnvExtras is a no-op on macOS: the LaunchAgent daemon already
// lives in the graphical login session.
func graphicalEnvExtras() []string { return nil }
