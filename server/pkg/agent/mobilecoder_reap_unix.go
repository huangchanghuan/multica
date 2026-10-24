//go:build !windows

package agent

// mobilecoder worker reaping. mobile run leaks a detached `__session-worker`
// daemon per headless run (verified against mobile 1.4.1): the worker
// double-forks out of the child's process group, is reparented to the user's
// init, idles at ~50% CPU and never exits. The CLI offers no flag or command
// that suppresses or stops it, so after each run the backend sweeps the
// user's stray workers for the resolved executable.

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// mobilecoderSessionWorkerArg is the marker argument a leaked Mobile Coder
// session worker runs under, as the final argv entry of the CLI process.
const mobilecoderSessionWorkerArg = "__session-worker"

// reapMobilecoderSessionWorkers terminates the user's stray
// `mobile-coder __session-worker` daemons whose executable path matches
// execPath. Best-effort and non-fatal: each miss is logged and skipped, and
// an unreadable /proc simply ends the sweep.
//
// Matching is on the exact resolved executable path plus the trailing
// __session-worker argv marker, so unrelated processes named "mobile" are
// never signalled. Killing a worker that an interactive TUI still wants only
// costs it a respawn on next use, which is far cheaper than a 50%-CPU zombie
// per headless run accumulating on the host.
func reapMobilecoderSessionWorkers(execPath string, logger *slog.Logger) {
	if execPath == "" || logger == nil {
		return
	}
	// Resolve both sides through /proc's exe symlink so a relative or
	// symlinked execPath still matches the worker's canonical path.
	entries, err := os.ReadDir("/proc")
	if err != nil {
		logger.Warn("mobilecoder worker reap: cannot read /proc", "error", err)
		return
	}
	self := os.Getpid()
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == self {
			continue
		}
		if !mobilecoderProcIsSessionWorker(pid, execPath) {
			continue
		}
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			logger.Warn("mobilecoder worker reap: kill failed", "pid", pid, "error", err)
			continue
		}
		logger.Info("mobilecoder worker reaped", "pid", pid)
	}
}

// mobilecoderProcIsSessionWorker reports whether pid is a `mobile …
// __session-worker` process whose executable resolves to the same file as
// execPath. A vanished or unreadable process is never a match.
func mobilecoderProcIsSessionWorker(pid int, execPath string) bool {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return false
	}
	fields := bytes.Split(bytes.TrimRight(raw, "\x00"), []byte{0})
	if len(fields) == 0 || !bytes.Equal(fields[len(fields)-1], []byte(mobilecoderSessionWorkerArg)) {
		return false
	}
	procExe, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
	if err != nil {
		return false
	}
	want, err := filepath.EvalSymlinks(execPath)
	if err != nil {
		want = execPath
	}
	// The kernel suffixes a deleted executable with " (deleted)"; trim it so
	// a mid-upgrade binary still matches its own workers.
	procExe = strings.TrimSuffix(procExe, " (deleted)")
	return procExe == want
}
