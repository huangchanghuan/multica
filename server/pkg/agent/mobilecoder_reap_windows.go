//go:build windows

package agent

// mobilecoder worker reaping is a Unix concern: the /proc scan this defence
// uses does not exist on Windows, and the leak itself has only been observed
// on Linux hosts (mobile 1.4.1 double-forks a __session-worker daemon out of
// the run's process group). Windows builds get a no-op so the call site in
// mobilecoder.go stays platform-free; if the leak is confirmed on Windows, a
// toolhelp32-based scan would be the replacement.

import "log/slog"

func reapMobilecoderSessionWorkers(execPath string, logger *slog.Logger) {}
