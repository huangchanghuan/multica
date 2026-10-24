package agent

// This file implements the Mobile Coder backend as a fully self-contained,
// independent agent, following the deveco.go precedent. Mobile Coder (the
// `mobile` CLI, China Mobile's enterprise fork of OpenCode) is a separate
// product maintained by a different company; it is intentionally kept
// decoupled from the OpenCode backend so a change to either agent can never
// affect the other. The two share only the package-wide generic process
// helpers (configureProcessGroup, filterCustomArgs, trySend, …) that every
// backend in this package reuses.
//
// Mobile Coder speaks the same `run --format json` protocol and emits the
// same NDJSON event stream as upstream OpenCode 1.x, so the event schema
// below mirrors that shape. Deliberate differences from the OpenCode
// integration (all verified against mobile 1.4.1):
//
//  1. `--yolo` instead of `--dangerously-skip-permissions` — the fork's
//     name for the daemon-managed skip-permissions flag. Passing the
//     upstream name makes the yargs CLI print usage and exit 1, so the
//     OpenCode backend can never drive this binary.
//
//  2. The prompt is delivered on stdin, never argv, inheriting the
//     OpenCode backend's defence against Windows CreateProcess lpCommandLine
//     truncation (#6538) and OS process-listing exposure. Mobile Coder's
//     `run` merges its variadic [message..] positional with piped stdin the
//     same way OpenCode does.
//
//  3. No version-derived protocol switching. Mobile Coder's version numbers
//     (1.4.x today) are its own release line and share no semantics with
//     upstream OpenCode versions; opencodeUsesV2Contract and
//     opencodeSeparatesReasoning must NOT be called here. If the fork ever
//     ships a "2.0.0" whose protocol actually changed, a mobilecoder-local
//    contract check will have to be designed then — mapping upstream
//    semantics onto this version string would silently activate the wrong
//    flags (dropping --dir, folding --variant into the model string).
//    Likewise the reasoning token counter is ignored: measured against
//    mobile 1.4.1 (glm-5.2 with and without --variant high) it reports 0
//    while output carries the full visible generation, so adding it would
//    only risk double counting if a later release follows upstream's
//    separated-reasoning convention.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// mobilecoderTerminateGraceNanos optionally overrides, in nanoseconds, how
// long a cancelled mobile process is given to exit after SIGTERM before it
// (and its whole process group) is SIGKILLed. Zero means use the default.
var mobilecoderTerminateGraceNanos atomic.Int64

func mobilecoderTerminateGrace() time.Duration {
	if n := mobilecoderTerminateGraceNanos.Load(); n > 0 {
		return time.Duration(n)
	}
	return 5 * time.Second
}

// mobilecoderBlockedArgs are flags hardcoded by the daemon that must not be
// overridden by user-configured custom_args. Mobile Coder's `run` subcommand
// exposes the same daemon-managed flags as OpenCode's, under the fork's own
// skip-permissions name.
var mobilecoderBlockedArgs = map[string]blockedArgMode{
	"--format":  blockedWithValue,  // json output format for daemon communication
	"--dir":     blockedWithValue,  // task workdir anchor for skill / AGENTS.md discovery
	"--variant": blockedWithValue,  // owned by agent.thinking_level
	"--yolo":    blockedStandalone, // daemon manages non-interactive permission prompts
}

// mobilecoderBackend implements Backend by spawning
// `mobile run --format json --yolo` and reading streaming JSON events from
// stdout.
type mobilecoderBackend struct {
	cfg Config
}

func (b *mobilecoderBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	execPath := b.cfg.ExecutablePath
	if execPath == "" {
		execPath = "mobile"
	}
	resolved, err := exec.LookPath(execPath)
	if err != nil {
		return nil, fmt.Errorf("mobilecoder executable not found at %q: %w", execPath, err)
	}
	execPath = resolved

	timeout := opts.Timeout
	runCtx, cancel := runContext(ctx, timeout)

	args := []string{"run", "--format", "json", "--yolo"}
	// Anchor Mobile Coder's project discovery (AGENTS.md walk-up +
	// .mobile-coder/skills project config scan) at the task workdir,
	// mirroring the OpenCode anchor: without --dir, the CLI falls back to
	// PWD (inherited from the daemon) or process.cwd(), which in self-host
	// deployments can resolve to the user's shell working directory and
	// silently bypass the per-task workdir. PWD is also overridden below for
	// the same reason.
	if opts.Cwd != "" {
		args = append(args, "--dir", opts.Cwd)
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.ThinkingLevel != "" {
		args = append(args, "--variant", opts.ThinkingLevel)
	}
	// Mobile Coder's `run` subcommand has no --prompt flag, so SystemPrompt
	// is intentionally not forwarded; system context is delivered via the
	// per-task AGENTS.md the daemon writes for mobilecoder.
	if opts.MaxTurns > 0 {
		b.cfg.Logger.Warn("mobilecoder does not support --max-turns; ignoring", "maxTurns", opts.MaxTurns)
	}
	if opts.ResumeSessionID != "" {
		args = append(args, "--session", opts.ResumeSessionID)
	}
	args = append(args, filterCustomArgs(opts.CustomArgs, mobilecoderBlockedArgs, b.cfg.Logger)...)
	// The task prompt is delivered on stdin, never argv — see the StdinPipe
	// wiring below. Keeping the prompt off argv keeps it under the Windows
	// lpCommandLine ceiling and out of OS process listings; the shared
	// command logger separately redacts argv values.

	runtimeCmd := b.cfg.commandAt(execPath)
	cmd := runtimeCmd.exec(runCtx, args...)
	hideAgentWindow(cmd)
	// Take over context cancellation: drive a graceful, group-wide
	// SIGTERM→SIGKILL from the cancellation goroutine below and close the
	// stdout read end only after the tree has been signalled. Returning nil
	// here keeps os/exec from racing us with its own kill; WaitDelay is the
	// hard backstop.
	cmd.Cancel = func() error { return nil }
	b.cfg.logAgentCommandWithPrompt(cmd, newAgentCommandLogArgs(args, trustAgentCommandPositional(0, "run")), len(prompt))
	cmd.WaitDelay = 10 * time.Second
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}

	env := buildEnv(b.cfg.Env)
	// Suppress the per-run __session-worker daemon at the source. Mobile
	// Coder's multi-process mode double-forks a detached session worker per
	// run (reparented to init, idling ~50% CPU, never exits — verified
	// against mobile 1.4.1), and the CLI documents MULTI_PROCESS_ENABLED=
	// false as its single-process fallback (the binary's own worker-entry
	// error path recommends it). A run under this flag completes normally
	// with no worker left behind. Only set it when the operator has not
	// pinned their own value through agent.custom_env; the post-run reap in
	// the result goroutine stays as the backstop for overrides and for
	// workers the flag does not cover.
	if _, pinned := b.cfg.Env["MULTI_PROCESS_ENABLED"]; !pinned {
		env = append(env, "MULTI_PROCESS_ENABLED=false")
	}
	// Override PWD so the child mobile process resolves its discovery root
	// to the task workdir. cmd.Dir alone is not enough: Mobile Coder reads
	// PWD (inherited from the parent daemon) before falling back to
	// process.cwd() when computing the directory it walks for AGENTS.md /
	// .mobile-coder/skills.
	if opts.Cwd != "" {
		env = append(env, "PWD="+opts.Cwd)
	}
	cmd.Env = env

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("mobilecoder stdout pipe: %w", err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("mobilecoder stdin pipe: %w", err)
	}
	var closeStdinOnce sync.Once
	closeStdin := func() { closeStdinOnce.Do(func() { _ = stdin.Close() }) }
	cmd.Stderr = newLogWriter(b.cfg.Logger, "[mobilecoder:stderr] ")

	if err := startOwnedProcessTree(cmd, b.cfg.Logger); err != nil {
		closeStdin()
		cancel()
		return nil, fmt.Errorf("start mobilecoder: %w", err)
	}

	b.cfg.Logger.Info("mobilecoder started", "pid", cmd.Process.Pid, "cwd", opts.Cwd, "model", opts.Model)

	msgCh := make(chan Message, 256)
	resCh := make(chan Result, 1)

	// procDone closes once cmd.Wait() returns, letting the cancellation
	// handler skip a process that already exited and avoid signalling a dead
	// pid.
	procDone := make(chan struct{})

	// Write the prompt from its own goroutine so it cannot deadlock against
	// the stdout reader below: a prompt larger than the OS pipe buffer
	// (~64 KiB) blocks mid-write until the CLI drains it, and the CLI cannot
	// drain while nobody is consuming its stdout. Closing stdin is what ends
	// the prompt — `mobile run` reads it to EOF, so a stdin left open hangs
	// the run forever. Close on every path, success or error.
	writeErrCh := make(chan error, 1)
	go func() {
		_, err := io.WriteString(stdin, prompt)
		closeStdin()
		writeErrCh <- err
	}()

	// On cancellation / timeout, terminate the process (and the tool
	// subprocesses it spawned) BEFORE unblocking the scanner. Closing the
	// stdout read end immediately would leave the CLI writing into a closed
	// pipe: every write returns EPIPE and the orphaned process can spin at
	// 100% CPU. Instead SIGTERM the whole process group, give it a grace
	// period to exit cleanly, then SIGKILL it. Only then is it safe to close
	// the stdout read end as a last-resort unblock for a scanner that a
	// wedged descendant still keeps open (#4533).
	go func() {
		select {
		case <-procDone:
			return // finished on its own; nothing to terminate
		case <-runCtx.Done():
		}
		// Release a prompt write still blocked on a full stdin pipe.
		closeStdin()
		if cmd.Process != nil {
			signalProcessGroup(cmd, syscall.SIGTERM)
			select {
			case <-procDone: // exited within the grace window
			case <-time.After(mobilecoderTerminateGrace()):
				signalProcessGroup(cmd, syscall.SIGKILL)
			}
		}
		_ = stdout.Close()
	}()

	go func() {
		defer cancel()
		defer close(msgCh)
		defer close(resCh)

		startTime := time.Now()
		scanResult := b.processEvents(stdout, msgCh)

		// Wait for process exit, then release the cancellation handler.
		exitErr := cmd.Wait()
		close(procDone)
		releaseProcessGroup(cmd)
		duration := time.Since(startTime)

		// mobile run leaks a detached `__session-worker` daemon per run
		// (verified against mobile 1.4.1: the worker double-forks out of the
		// child's process group, is reparented to the user's init, idles at
		// ~50% CPU and never exits). The CLI offers no flag or command that
		// suppresses or stops it, so after a successful run the backend
		// sweeps the user's stray workers for this resolved executable. The
		// match is on /proc/<pid>/cmdline ending in "__session-worker" with
		// the same executable path, so an interactive TUI's workers are
		// distinguishable only by their executable — and the TUI spawns the
		// same ones; killing a worker the TUI still needs only costs it a
		// respawn on next use, which is far cheaper than a 50%-CPU zombie
		// per headless run. Best-effort: failures are logged, not fatal.
		if scanResult.status == "completed" {
			reapMobilecoderSessionWorkers(execPath, b.cfg.Logger)
		}

		// Wait closes the process pipes, so a prompt write still blocked when
		// the CLI exited has returned by now. The writer sends exactly once.
		writeErr := <-writeErrCh

		if runCtx.Err() == context.DeadlineExceeded {
			scanResult.status = "timeout"
			scanResult.errMsg = fmt.Sprintf("mobilecoder timed out after %s", timeout)
		} else if runCtx.Err() == context.Canceled {
			scanResult.status = "aborted"
			scanResult.errMsg = "execution cancelled"
		} else if exitErr != nil && scanResult.status == "completed" {
			scanResult.status = "failed"
			scanResult.errMsg = fmt.Sprintf("mobilecoder exited with error: %v", exitErr)
		} else if writeErr != nil && !scanResult.sawTerminalSignal {
			// A failed prompt write is only benign once the run is PROVEN to
			// have finished: the CLI reads stdin to EOF before it does any
			// work, so a run that reached a terminal signal necessarily
			// received the whole prompt, and an EPIPE recorded after that
			// just means the pipe closed on its way out. Absence of failure
			// is not that proof — a child that emits nothing and exits 0
			// still reports "completed", and if the prompt never landed that
			// is precisely the run we must not pass off as a clean success.
			if scanResult.errMsg == "" {
				scanResult.errMsg = fmt.Sprintf("mobilecoder prompt write failed: %v", writeErr)
			} else {
				scanResult.errMsg = fmt.Sprintf("%s; mobilecoder prompt write failed: %v", scanResult.errMsg, writeErr)
			}
			scanResult.status = "failed"
		}

		b.cfg.Logger.Info("mobilecoder finished", "pid", cmd.Process.Pid, "status", scanResult.status, "duration", duration.Round(time.Millisecond).String())

		// Build usage map. Mobile Coder doesn't report model per-step, so we
		// attribute all usage to the configured model (or "unknown").
		var usage map[string]TokenUsage
		u := scanResult.usage
		if u.InputTokens > 0 || u.OutputTokens > 0 || u.CacheReadTokens > 0 || u.CacheWriteTokens > 0 {
			model := opts.Model
			if model == "" {
				model = "unknown"
			}
			usage = map[string]TokenUsage{model: u}
		}

		resCh <- Result{
			Status:     scanResult.status,
			Output:     scanResult.output,
			Error:      scanResult.errMsg,
			DurationMs: duration.Milliseconds(),
			SessionID:  scanResult.sessionID,
			Usage:      usage,
		}
	}()

	return &Session{Messages: msgCh, Result: resCh}, nil
}

// ── Event handlers ──

// mobilecoderEventResult holds the accumulated state from processing the
// event stream.
type mobilecoderEventResult struct {
	status    string
	errMsg    string
	output    string
	sessionID string
	usage     TokenUsage // accumulated token usage across all steps
	// sawTerminalSignal is positive evidence that the run actually finished:
	// a step_finish closed a step with something to show for it. It gates
	// whether a failed stdin write is treated as a failed run (see Execute).
	sawTerminalSignal bool
}

// processEvents reads JSON lines from r, dispatches events to ch, and
// returns the accumulated result. Extracted for testability.
func (b *mobilecoderBackend) processEvents(r io.Reader, ch chan<- Message) mobilecoderEventResult {
	var output strings.Builder
	var sessionID string
	var usage TokenUsage
	finalStatus := "completed"
	var finalError string

	// Track step bracketing so a stream that ends mid-step is not mistaken
	// for a clean completion — the same fail-closed guard the OpenCode
	// backend applies: `mobile run` has no terminal result event, so "no
	// error seen" is not proof the run finished. A step that opens and never
	// closes (or a run that never opens a step at all) fails closed.
	openStep := false
	sawStepFinish := false

	scanner := newAgentStreamScanner(r)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var event mobilecoderEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}

		if event.SessionID != "" {
			sessionID = event.SessionID
		}

		switch event.Type {
		case "text":
			b.handleTextEvent(event, ch, &output)
		case "tool_use":
			b.handleToolUseEvent(event, ch)
		case "error":
			b.handleErrorEvent(event, ch, &finalStatus, &finalError)
		case "step_start":
			openStep = true
			trySend(ch, Message{Type: MessageStatus, Status: "running"})
		case "step_finish":
			openStep = false
			sawStepFinish = true
			// Accumulate token usage from step_finish events. reasoning is
			// deliberately not modeled: measured on mobile 1.4.1 it reports 0
			// with output already carrying the full generation.
			if t := event.Part.Tokens; t != nil {
				usage.InputTokens += t.Input
				usage.OutputTokens += t.Output
				if t.Cache != nil {
					usage.CacheReadTokens += t.Cache.Read
					usage.CacheWriteTokens += t.Cache.Write
				}
			}
		}
	}

	if scanErr := scanner.Err(); scanErr != nil {
		b.cfg.Logger.Warn("mobilecoder stdout scanner error", "error", scanErr)
		if finalStatus == "completed" {
			finalStatus = "failed"
			finalError = fmt.Sprintf("stdout read error: %v", scanErr)
		}
	}

	// Fail closed on a stream that ended without a terminal signal: a clean
	// EOF while a step is still open, or a run that never closed any step,
	// means the provider stream died and `mobile run` exited without an
	// error event. The wording keeps the "… stream ended without a terminal
	// signal" phrase that taskfailure classify matches for retry
	// classification — a mobilecoder-prefixed variant of the opencode
	// prefix family.
	noTerminalSignal := false
	if finalStatus == "completed" && (openStep || !sawStepFinish) {
		finalStatus = "failed"
		if openStep {
			finalError = "mobilecoder stream ended without a terminal signal (step still open at EOF)"
		} else {
			finalError = "mobilecoder stream ended without a terminal signal (no step finished)"
		}
		noTerminalSignal = true
	}

	return mobilecoderEventResult{
		status:            finalStatus,
		errMsg:            finalError,
		output:            output.String(),
		sessionID:         sessionID,
		usage:             usage,
		sawTerminalSignal: sawStepFinish && !noTerminalSignal,
	}
}

func (b *mobilecoderBackend) handleTextEvent(event mobilecoderEvent, ch chan<- Message, output *strings.Builder) {
	text := event.Part.Text
	if text != "" {
		output.WriteString(text)
		trySend(ch, Message{Type: MessageText, Content: text})
	}
}

// handleToolUseEvent processes "tool_use" events. A single tool_use event
// contains both the call and result in part.state when the tool reaches a
// terminal state (state.status is "completed" or "error").
func (b *mobilecoderBackend) handleToolUseEvent(event mobilecoderEvent, ch chan<- Message) {
	var input map[string]any
	if event.Part.State != nil && event.Part.State.Input != nil {
		_ = json.Unmarshal(event.Part.State.Input, &input)
	}

	trySend(ch, Message{
		Type:   MessageToolUse,
		Tool:   event.Part.Tool,
		CallID: event.Part.CallID,
		Input:  input,
	})

	// Pair every terminal tool-use with a tool-result. The daemon uses this
	// pair to track in-flight tools, so dropping error results would leave
	// its counter permanently elevated and suppress the normal idle
	// watchdog.
	state := event.Part.State
	if state != nil && (state.Status == "completed" || state.Status == "error") {
		outputStr := extractMobilecoderToolOutput(state.Output)
		if state.Status == "error" && state.Error != "" {
			outputStr = state.Error
		}
		trySend(ch, Message{
			Type:   MessageToolResult,
			Tool:   event.Part.Tool,
			CallID: event.Part.CallID,
			Output: outputStr,
		})
	}
}

// handleErrorEvent processes "error" events. The CLI can exit with RC=0
// even on errors (e.g. invalid model), so error events are the reliable
// failure signal.
func (b *mobilecoderBackend) handleErrorEvent(event mobilecoderEvent, ch chan<- Message, finalStatus, finalError *string) {
	errMsg := ""
	if event.Error != nil {
		errMsg = event.Error.Message()
	}
	if errMsg == "" {
		errMsg = "unknown mobilecoder error"
	}

	b.cfg.Logger.Warn("mobilecoder error event", "error", errMsg)
	trySend(ch, Message{Type: MessageError, Content: errMsg})

	*finalStatus = "failed"
	*finalError = errMsg
}

// extractMobilecoderToolOutput converts the tool state output (which may be
// a string or structured object) into a string.
func extractMobilecoderToolOutput(output any) string {
	if output == nil {
		return ""
	}
	if s, ok := output.(string); ok {
		return s
	}
	data, _ := json.Marshal(output)
	return string(data)
}

// ── JSON types for `mobile run --format json` stdout events ──
//
// Mobile Coder emits the same NDJSON event schema as upstream OpenCode 1.x
// (verified against mobile 1.4.1). Event types observed in real output:
//
//	"step_start"  — agent step begins
//	"text"        — text output from agent (part.text)
//	"tool_use"    — tool invocation with call and result (part.tool, part.callID, part.state)
//	"error"       — error from the CLI (error.name, error.data.message)
//	"step_finish" — agent step completes (includes token usage)
type mobilecoderEvent struct {
	Type      string               `json:"type"`
	Timestamp int64                `json:"timestamp,omitempty"`
	SessionID string               `json:"sessionID,omitempty"`
	Part      mobilecoderEventPart `json:"part"`
	Error     *mobilecoderError    `json:"error,omitempty"`
}

// mobilecoderEventPart represents the part field in a mobilecoder event.
type mobilecoderEventPart struct {
	ID        string `json:"id,omitempty"`
	MessageID string `json:"messageID,omitempty"`
	SessionID string `json:"sessionID,omitempty"`
	Type      string `json:"type,omitempty"`

	// Text events
	Text string `json:"text,omitempty"`

	// Tool use events
	Tool   string                `json:"tool,omitempty"`
	CallID string                `json:"callID,omitempty"`
	State  *mobilecoderToolState `json:"state,omitempty"`
	// OpenCode-lineage CLIs exclude provider-executed tools when deciding
	// whether a tool result requires another model step.
	Metadata *mobilecoderPartMetadata `json:"metadata,omitempty"`

	// step_finish token usage
	Tokens *mobilecoderTokens `json:"tokens,omitempty"`

	// step_finish reason (FinishReason: "stop", "tool-calls", …).
	Reason string `json:"reason,omitempty"`
}

type mobilecoderPartMetadata struct {
	ProviderExecuted bool `json:"providerExecuted,omitempty"`
}

// mobilecoderTokens represents token usage in a step_finish event.
type mobilecoderTokens struct {
	Input     int64                   `json:"input"`
	Output    int64                   `json:"output"`
	Reasoning int64                   `json:"reasoning,omitempty"`
	Total     int64                   `json:"total,omitempty"`
	Cache     *mobilecoderCacheTokens `json:"cache,omitempty"`
}

type mobilecoderCacheTokens struct {
	Read  int64 `json:"read"`
	Write int64 `json:"write"`
}

// mobilecoderToolState represents the state of a tool invocation.
type mobilecoderToolState struct {
	Status string          `json:"status,omitempty"`
	Input  json.RawMessage `json:"input,omitempty"`
	Output any             `json:"output,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// mobilecoderError represents an error event from the CLI.
type mobilecoderError struct {
	Name string              `json:"name,omitempty"`
	Data *mobilecoderErrData `json:"data,omitempty"`
	// Flat 2.x-style shape some opencode forks report even on 1.x lines.
	Type string `json:"type,omitempty"`
	Msg  string `json:"message,omitempty"`
}

// Message returns the human-readable error message.
func (e *mobilecoderError) Message() string {
	if e.Data != nil && e.Data.Message != "" {
		return e.Data.Message
	}
	if e.Msg != "" {
		if e.Type != "" {
			return e.Type + ": " + e.Msg
		}
		return e.Msg
	}
	if e.Name != "" {
		return e.Name
	}
	return e.Type
}

type mobilecoderErrData struct {
	Message string `json:"message,omitempty"`
}
