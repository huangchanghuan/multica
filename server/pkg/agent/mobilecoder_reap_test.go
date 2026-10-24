package agent

// Tests for the mobilecoder session-worker defences. Unix-only: the reaper
// itself is behind !windows; the MULTI_PROCESS_ENABLED injection test needs
// /bin/sh fixtures.

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"log/slog"
)

// runMobilecoderRecordingScript executes the mobilecoder backend against a
// fake CLI that appends its visible MULTI_PROCESS_ENABLED / PWD environment
// to an env file, emits a minimal completed event stream, and exits. pinned
// (may be nil) is merged into cfg.Env before Execute so the operator-override
// path is exercised through the real buildEnv plumbing. It returns the
// recorded env lines.
func runMobilecoderRecordingScript(t *testing.T, pinned map[string]string) []string {
	t.Helper()
	tempDir := t.TempDir()
	envFile := filepath.Join(tempDir, "env.txt")
	fakePath := filepath.Join(tempDir, "mobile")
	script := `#!/bin/sh
if [ -n "$MOBILECODER_ENV_FILE" ]; then
  env | grep -E '^(MULTI_PROCESS_ENABLED|PWD)=' >> "$MOBILECODER_ENV_FILE" || true
fi
printf '{"type":"step_start","sessionID":"s","part":{"type":"step-start"}}\n'
printf '{"type":"text","sessionID":"s","part":{"type":"text","text":"ok"}}\n'
printf '{"type":"step_finish","sessionID":"s","part":{"type":"step-finish","reason":"stop","tokens":{"input":1,"output":1}}}\n'
`
	writeTestExecutable(t, fakePath, []byte(script))

	cfgEnv := map[string]string{"MOBILECODER_ENV_FILE": envFile}
	for k, v := range pinned {
		cfgEnv[k] = v
	}
	backend, err := New("mobilecoder", Config{
		ExecutablePath: fakePath,
		Logger:         slog.Default(),
		Env:            cfgEnv,
	})
	if err != nil {
		t.Fatalf("new mobilecoder backend: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "x", ExecOptions{Cwd: t.TempDir(), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	go func() {
		for range session.Messages {
		}
	}()
	if res := <-session.Result; res.Status != "completed" {
		t.Fatalf("result status = %q, error = %q", res.Status, res.Error)
	}
	raw, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}
	return splitNonEmptyLines(string(raw))
}

// TestMobilecoderEnvSuppressesSessionWorker pins the first line of defence
// against the per-run __session-worker leak: MULTI_PROCESS_ENABLED=false is
// injected into the child environment by default.
func TestMobilecoderEnvSuppressesSessionWorker(t *testing.T) {
	t.Parallel()

	lines := runMobilecoderRecordingScript(t, nil)
	if !containsString(lines, "MULTI_PROCESS_ENABLED=false") {
		t.Errorf("default env missing MULTI_PROCESS_ENABLED=false: %v", lines)
	}
}

// TestMobilecoderEnvRespectsPinnedMultiProcess: when the operator pinned
// their own MULTI_PROCESS_ENABLED through agent.custom_env, the backend must
// not override it with false.
func TestMobilecoderEnvRespectsPinnedMultiProcess(t *testing.T) {
	t.Parallel()

	lines := runMobilecoderRecordingScript(t, map[string]string{"MULTI_PROCESS_ENABLED": "true"})
	if containsString(lines, "MULTI_PROCESS_ENABLED=false") {
		t.Errorf("operator-pinned MULTI_PROCESS_ENABLED=true was overridden by the backend: %v", lines)
	}
	if !containsString(lines, "MULTI_PROCESS_ENABLED=true") {
		t.Errorf("operator-pinned MULTI_PROCESS_ENABLED=true missing from child env: %v", lines)
	}
}

// procAliveViaProc reports whether pid has a living (non-zombie) /proc
// entry. A SIGKILLed child whose Wait is parked in a goroutine may linger
// briefly as a zombie; that already counts as reaped for these tests.
func procAliveViaProc(pid int) bool {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	fields := strings.Fields(string(raw))
	if len(fields) < 3 {
		return false
	}
	return fields[2] != "Z"
}

// TestReapMobilecoderSessionWorkersKillsOnlyMatchingWorkers spawns (1) a real
// leaked-worker impersonator — a sleep process whose argv ends in
// __session-worker and whose exe is a copy of the test binary — and (2) an
// innocent sleep process. The reaper must kill the first and leave the
// second, and must not touch processes whose executable differs even when
// their argv carries the marker.
func TestReapMobilecoderSessionWorkersKillsOnlyMatchingWorkers(t *testing.T) {
	if os.Getenv("MC_REAP_TEST_HELPER") == "1" {
		t.Skip("guard against recursive self-run")
	}

	tmp := t.TempDir()
	// Two distinct executables so the exe-path match is exercised: an exact
	// copy of this test binary stands in for the resolved mobile binary.
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	mobileExe := filepath.Join(tmp, "mobile")
	if data, err := os.ReadFile(self); err == nil {
		if err := os.WriteFile(mobileExe, data, 0o755); err != nil {
			t.Fatalf("write fake mobile exe: %v", err)
		}
	}

	worker := spawnSleepImpersonator(t, mobileExe, "__session-worker")
	otherExeWorker := spawnSleepImpersonator(t, self, "__session-worker")
	innocent := spawnSleepImpersonator(t, self, "innocent-arg")
	defer func() {
		for _, p := range []*os.Process{worker, otherExeWorker, innocent} {
			if p != nil {
				_ = p.Kill()
			}
		}
	}()

	reapMobilecoderSessionWorkers(mobileExe, slog.Default())

	deadline := time.Now().Add(5 * time.Second)
	for procAliveViaProc(worker.Pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if procAliveViaProc(worker.Pid) {
		t.Errorf("matching __session-worker (pid %d) survived the reap", worker.Pid)
	}
	if !procAliveViaProc(otherExeWorker.Pid) {
		t.Errorf("worker with a different executable (pid %d) was killed; the reap must match on exe path", otherExeWorker.Pid)
	}
	if !procAliveViaProc(innocent.Pid) {
		t.Errorf("process without the __session-worker marker (pid %d) was killed", innocent.Pid)
	}
}

// TestReapMobilecoderSessionWorkersEmptyPathIsNoop pins the guards: an empty
// execPath returns without touching /proc.
func TestReapMobilecoderSessionWorkersEmptyPathIsNoop(t *testing.T) {
	reapMobilecoderSessionWorkers("", slog.Default()) // must not panic
}
