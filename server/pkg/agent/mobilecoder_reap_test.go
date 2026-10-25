package agent

// Tests for the mobilecoder session-worker defences. Unix-only: the reaper
// itself is behind !windows; the MULTI_PROCESS_ENABLED injection test needs
// /bin/sh fixtures.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"log/slog"
)

// runMobilecoderRecordingScript executes the mobilecoder backend against a
// fake CLI that appends its visible MULTI_PROCESS_ENABLED / PWD /
// MOBILE_CODER_CONFIG_CONTENT environment to an env file, emits a minimal
// completed event stream, and exits. pinned (may be nil) is merged into
// cfg.Env before Execute so the operator-override path is exercised through
// the real buildEnv plumbing; mcpRaw (may be nil) is passed as
// ExecOptions.McpConfig. It returns the recorded env lines.
func runMobilecoderRecordingScript(t *testing.T, pinned map[string]string, mcpRaw json.RawMessage) []string {
	t.Helper()
	tempDir := t.TempDir()
	envFile := filepath.Join(tempDir, "env.txt")
	fakePath := filepath.Join(tempDir, "mobile")
	script := `#!/bin/sh
if [ -n "$MOBILECODER_ENV_FILE" ]; then
  env | grep -E '^(MULTI_PROCESS_ENABLED|PWD|MOBILE_CODER_CONFIG_CONTENT)=' >> "$MOBILECODER_ENV_FILE" || true
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
	session, err := backend.Execute(ctx, "x", ExecOptions{Cwd: t.TempDir(), Timeout: 5 * time.Second, McpConfig: mcpRaw})
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

	lines := runMobilecoderRecordingScript(t, nil, nil)
	if !containsString(lines, "MULTI_PROCESS_ENABLED=false") {
		t.Errorf("default env missing MULTI_PROCESS_ENABLED=false: %v", lines)
	}
	if containsString(lines, "MOBILE_CODER_CONFIG_CONTENT=") {
		t.Errorf("MOBILE_CODER_CONFIG_CONTENT injected without any mcp_config: %v", lines)
	}
}

// TestMobilecoderEnvRespectsPinnedMultiProcess: when the operator pinned
// their own MULTI_PROCESS_ENABLED through agent.custom_env, the backend must
// not override it with false.
func TestMobilecoderEnvRespectsPinnedMultiProcess(t *testing.T) {
	t.Parallel()

	lines := runMobilecoderRecordingScript(t, map[string]string{"MULTI_PROCESS_ENABLED": "true"}, nil)
	if containsString(lines, "MULTI_PROCESS_ENABLED=false") {
		t.Errorf("operator-pinned MULTI_PROCESS_ENABLED=true was overridden by the backend: %v", lines)
	}
	if !containsString(lines, "MULTI_PROCESS_ENABLED=true") {
		t.Errorf("operator-pinned MULTI_PROCESS_ENABLED=true missing from child env: %v", lines)
	}
}

// TestMobilecoderEnvInjectsMCPAndSkipsSuppression pins the MCP channel and
// its mutual exclusion with single-process mode: a configured mcp_config is
// forwarded via MOBILE_CODER_CONFIG_CONTENT, and the MULTI_PROCESS_ENABLED=
// false suppression is skipped because the fork can only serve MCP from its
// multi-process session manager (a single-process run with an injected
// config dies silently after step_start — verified against mobile 1.4.1).
func TestMobilecoderEnvInjectsMCPAndSkipsSuppression(t *testing.T) {
	t.Parallel()

	mcp := json.RawMessage(`{"mcp":{"canary":{"type":"local","command":["/bin/true"]}}}`)
	lines := runMobilecoderRecordingScript(t, nil, mcp)
	// The translator re-marshals the validated entries, so map keys come out
	// sorted — assert against that deterministic form.
	if !containsString(lines, `MOBILE_CODER_CONFIG_CONTENT={"mcp":{"canary":{"command":["/bin/true"],"type":"local"}}}`) {
		t.Errorf("MOBILE_CODER_CONFIG_CONTENT missing or wrong in child env: %v", lines)
	}
	if containsString(lines, "MULTI_PROCESS_ENABLED=false") {
		t.Errorf("worker suppression was injected alongside MCP; the two channels are mutually exclusive: %v", lines)
	}
}

// TestMobilecoderMCPWithPinnedMultiProcessFailsFast: an operator-pinned
// MULTI_PROCESS_ENABLED together with agent.mcp_config is a guaranteed
// silent failure (the run dies after step_start, exit 0, no diagnostics), so
// the backend must refuse the run before spawning instead of burning it.
func TestMobilecoderMCPWithPinnedMultiProcessFailsFast(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	ranMarker := filepath.Join(tempDir, "ran")
	fakePath := filepath.Join(tempDir, "mobile")
	// The fake CLI records its invocation; if Execute fails fast this never
	// runs and the marker must not exist.
	script := `#!/bin/sh
touch "$MOBILECODER_RAN_MARKER"
printf '{"type":"step_start","sessionID":"s","part":{"type":"step-start"}}\n'
`
	writeTestExecutable(t, fakePath, []byte(strings.ReplaceAll(script, "$MOBILECODER_RAN_MARKER", ranMarker)))

	backend, err := New("mobilecoder", Config{
		ExecutablePath: fakePath,
		Logger:         slog.Default(),
		Env:            map[string]string{"MULTI_PROCESS_ENABLED": "true"},
	})
	if err != nil {
		t.Fatalf("new mobilecoder backend: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	mcp := json.RawMessage(`{"mcp":{"canary":{"type":"local","command":["/bin/true"]}}}`)
	if _, err := backend.Execute(ctx, "x", ExecOptions{Cwd: t.TempDir(), Timeout: 5 * time.Second, McpConfig: mcp}); err == nil {
		t.Fatalf("Execute with pinned MULTI_PROCESS_ENABLED + mcp_config succeeded; want fail-fast error")
	} else if !strings.Contains(err.Error(), "MULTI_PROCESS_ENABLED") {
		t.Errorf("fail-fast error %q does not mention MULTI_PROCESS_ENABLED", err)
	}
	if _, err := os.Stat(ranMarker); err == nil {
		t.Errorf("fake CLI ran despite the fail-fast rejection")
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

	// Candidates mirror the real call site: the shim path (a name that does
	// NOT exist as a binary — EvalSymlinks falls back to the literal path,
	// matching nothing) plus the real binary. The worker must still be
	// reaped through the real-binary candidate.
	shimPath := filepath.Join(tmp, "shim", "mobile")
	reapMobilecoderSessionWorkers([]string{shimPath, mobileExe}, slog.Default())

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

// TestReapMobilecoderSessionWorkersEmptyPathIsNoop pins the guards: no
// usable candidate returns without touching /proc.
func TestReapMobilecoderSessionWorkersEmptyPathIsNoop(t *testing.T) {
	reapMobilecoderSessionWorkers(nil, slog.Default())                 // must not panic
	reapMobilecoderSessionWorkers([]string{"", ""}, slog.Default())    // must not panic
	reapMobilecoderSessionWorkers([]string{"/definitely/absent"}, nil) // nil logger must not panic
}

