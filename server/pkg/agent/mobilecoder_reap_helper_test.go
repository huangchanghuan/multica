package agent

// Helpers for the mobilecoder reaper tests: spawn a child that impersonates
// a leaked __session-worker (argv marker + long sleep) so /proc scans can
// match against it, without depending on the real mobile binary.

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// reapHelperEnvVal marks the child re-exec running as an impersonator. The
// child side dispatch lives in TestMain (claude_deadlock_test.go), which runs
// before the testing flag parser and can therefore receive arbitrary argv
// such as a bare "__session-worker" marker.
const reapHelperEnv = "MC_REAP_TEST_HELPER"

// spawnSleepImpersonator starts exe with the given argv marker and a
// MC_REAP_TEST_HELPER env, where it sleeps for 60s (stand-in for a leaked
// worker). The child is its own process group so the test binary's cleanup
// never signals it accidentally.
func spawnSleepImpersonator(tb testing.TB, exe, markerArg string) *os.Process {
	tb.Helper()
	cmd := exec.Command(exe, markerArg)
	cmd.Env = append(os.Environ(), reapHelperEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		tb.Fatalf("spawn impersonator %s %s: %v", exe, markerArg, err)
	}
	// Reap-guard: the test never Waits, so park a background Wait to avoid
	// a zombie confusing os.FindProcess checks.
	go func() { _ = cmd.Wait() }()
	// Give the kernel a moment to materialise /proc/<pid>.
	time.Sleep(100 * time.Millisecond)
	return cmd.Process
}

// errorsIsProcessGone reports whether err means the process no longer exists
// (or is a zombie), for the reaper test's liveness probe.
func errorsIsProcessGone(err error) bool {
	return err != nil && (os.IsNotExist(err) || err.Error() == "os: process already finished")
}
