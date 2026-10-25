//go:build windows

package daemon

import "syscall"

// Windows CREATE_* constants missing from the syscall package. See
// cmd/multica/cmd_daemon_windows.go for the same constants used when spawning
// the daemon itself.
const (
	editorDetachedProcess      = 0x00000008
	editorCreateNewProcessGrp  = 0x00000200
	editorCreateBreakawayFrJob = 0x01000000
)

// editorSpawnSysProcAttr detaches the editor from the daemon's console and
// process group, and breaks away from the daemon's Job Object so closing the
// daemon's job does not close the editor. HideWindow is off: editor CLIs
// (code, cursor, …) exit immediately after handing off to the running
// instance, so there is no lingering console either way, and some of them
// print errors on stdout that a hidden console would swallow silently.
func editorSpawnSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: editorDetachedProcess |
			editorCreateNewProcessGrp |
			editorCreateBreakawayFrJob,
	}
}

// graphicalEnvExtras is a no-op on Windows: desktop processes get the
// window station from the session, not from environment variables.
func graphicalEnvExtras() []string { return nil }
