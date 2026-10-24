package agent

// Tests for the Mobile Coder backend. Self-contained: they use a
// mobilecoder-specific fake script and mobilecoderEvent fixtures, and reuse
// only the package-wide generic test helpers (writeTestExecutable,
// containsString, …) that every backend's tests share. They do not depend on
// the OpenCode backend or its test fixtures.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"log/slog"
)

// fakeMobilecoderScript impersonates the `mobile` CLI: it records argv to
// $MOBILECODER_ARGS_FILE and the stdin payload to $MOBILECODER_STDIN_FILE,
// then emits a minimal completed step on stdout so the backend's event loop
// terminates, and exits 0. Reading stdin to EOF mirrors the real CLI's
// `run` behaviour (the prompt arrives on stdin, and closing it is what ends
// the message).
func fakeMobilecoderScript() string {
	return `#!/bin/sh
if [ -n "$MOBILECODER_ARGS_FILE" ]; then
  for arg in "$@"; do
    printf '%s\n' "$arg" >> "$MOBILECODER_ARGS_FILE"
  done
fi
if [ -n "$MOBILECODER_STDIN_FILE" ]; then
  cat > "$MOBILECODER_STDIN_FILE"
fi
printf '{"type":"step_start","timestamp":1,"sessionID":"ses_fake","part":{"type":"step-start"}}\n'
printf '{"type":"text","timestamp":2,"sessionID":"ses_fake","part":{"type":"text","text":"ok"}}\n'
printf '{"type":"step_finish","timestamp":3,"sessionID":"ses_fake","part":{"type":"step-finish","reason":"stop","tokens":{"total":10,"input":7,"output":3,"reasoning":0,"cache":{"read":0,"write":0}}}}\n'
`
}

// TestMobilecoderBackendArgvShapeAndStdinPrompt pins the Mobile Coder
// differences from the OpenCode backend: argv is
// `run --format json --yolo --dir <wd> [--model ...] [--variant ...]
// [--session ...]` with NO positional prompt — the prompt travels on stdin —
// and the skip-permissions flag is the fork's `--yolo`, never
// `--dangerously-skip-permissions` (which the yargs CLI rejects with exit 1).
func TestMobilecoderBackendArgvShapeAndStdinPrompt(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	argsFile := filepath.Join(tempDir, "argv.txt")
	stdinFile := filepath.Join(tempDir, "stdin.txt")
	fakePath := filepath.Join(tempDir, "mobile")
	writeTestExecutable(t, fakePath, []byte(fakeMobilecoderScript()))

	workDir := t.TempDir()

	backend, err := New("mobilecoder", Config{
		ExecutablePath: fakePath,
		Logger:         slog.Default(),
		Env: map[string]string{
			"MOBILECODER_ARGS_FILE":  argsFile,
			"MOBILECODER_STDIN_FILE": stdinFile,
		},
	})
	if err != nil {
		t.Fatalf("new mobilecoder backend: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	session, err := backend.Execute(ctx, "do the thing", ExecOptions{
		Cwd:     workDir,
		Model:   "jiutian-moma/z.ai/glm-5.2",
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	go func() {
		for range session.Messages {
		}
	}()
	res := <-session.Result
	if res.Status != "completed" {
		t.Fatalf("result status = %q, error = %q; want completed", res.Status, res.Error)
	}

	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args file: %v", err)
	}
	args := splitNonEmptyLines(string(raw))

	if len(args) < 2 || args[0] != "run" {
		t.Fatalf("expected first arg 'run', got %v", args)
	}
	if !containsAdjacent(args, "--format", "json") {
		t.Errorf("expected --format json in argv: %v", args)
	}
	if !containsString(args, "--yolo") {
		t.Errorf("expected --yolo in argv: %v", args)
	}
	if containsString(args, "--dangerously-skip-permissions") {
		t.Errorf("argv must NOT contain --dangerously-skip-permissions (mobile rejects it): %v", args)
	}
	if !containsAdjacent(args, "--dir", workDir) {
		t.Errorf("expected --dir %q in argv: %v", workDir, args)
	}
	if !containsAdjacent(args, "--model", "jiutian-moma/z.ai/glm-5.2") {
		t.Errorf("expected --model jiutian-moma/z.ai/glm-5.2 in argv: %v", args)
	}
	if containsString(args, "do the thing") {
		t.Errorf("argv must NOT contain the prompt (it travels on stdin): %v", args)
	}

	stdinRaw, err := os.ReadFile(stdinFile)
	if err != nil {
		t.Fatalf("read stdin file: %v", err)
	}
	if strings.TrimSpace(string(stdinRaw)) != "do the thing" {
		t.Errorf("stdin payload = %q, want the full prompt", string(stdinRaw))
	}
}

// TestMobilecoderLargePromptStdin exercises the stdin write path with a
// payload well over the OS pipe buffer (~64 KiB): the writer goroutine must
// not deadlock against the stdout reader, and the full prompt must arrive.
func TestMobilecoderLargePromptStdin(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	stdinFile := filepath.Join(tempDir, "stdin.txt")
	fakePath := filepath.Join(tempDir, "mobile")
	writeTestExecutable(t, fakePath, []byte(fakeMobilecoderScript()))

	workDir := t.TempDir()
	prompt := strings.Repeat("移动编码器提示词负载。", 12*1024) // ~256 KiB

	backend, err := New("mobilecoder", Config{
		ExecutablePath: fakePath,
		Logger:         slog.Default(),
		Env:            map[string]string{"MOBILECODER_STDIN_FILE": stdinFile},
	})
	if err != nil {
		t.Fatalf("new mobilecoder backend: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	session, err := backend.Execute(ctx, prompt, ExecOptions{
		Cwd:     workDir,
		Timeout: 15 * time.Second,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	go func() {
		for range session.Messages {
		}
	}()
	res := <-session.Result
	if res.Status != "completed" {
		t.Fatalf("result status = %q, error = %q; want completed", res.Status, res.Error)
	}

	stdinRaw, err := os.ReadFile(stdinFile)
	if err != nil {
		t.Fatalf("read stdin file: %v", err)
	}
	if string(stdinRaw) != prompt {
		t.Errorf("stdin payload length = %d, want %d (full prompt)", len(stdinRaw), len(prompt))
	}
}

func TestNewReturnsMobilecoderBackend(t *testing.T) {
	t.Parallel()
	b, err := New("mobilecoder", Config{ExecutablePath: "/nonexistent/mobile"})
	if err != nil {
		t.Fatalf("New(mobilecoder) error: %v", err)
	}
	if _, ok := b.(*mobilecoderBackend); !ok {
		t.Fatalf("expected *mobilecoderBackend, got %T", b)
	}
}

// ── Event parsing tests with a real `mobile run --format json` fixture ──

func TestMobilecoderEventParsingTextFixture(t *testing.T) {
	t.Parallel()

	// Real output captured from `mobile run --format json --yolo` (1.4.1).
	line := `{"type":"text","timestamp":1792812593066,"sessionID":"ses_e940b5239ffeg6e4oEXeKdhLmO","part":{"id":"prt_16bf52c9a001GVbwiLNEPQA239","sessionID":"ses_e940b5239ffeg6e4oEXeKdhLmO","messageID":"msg_16bf4caf8001wDfknQ5be0SGJp","type":"text","text":"收到","time":{"start":1792812593057,"end":1792812593057}}}`

	var event mobilecoderEvent
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if event.Type != "text" {
		t.Errorf("type: got %q, want %q", event.Type, "text")
	}
	if event.SessionID != "ses_e940b5239ffeg6e4oEXeKdhLmO" {
		t.Errorf("sessionID: got %q", event.SessionID)
	}
	if event.Part.Text != "收到" {
		t.Errorf("part.text: got %q, want %q", event.Part.Text, "收到")
	}
}

func TestMobilecoderEventParsingStepFinishTokensFixture(t *testing.T) {
	t.Parallel()

	// Real output captured from `mobile run --format json --yolo` (1.4.1).
	line := `{"type":"step_finish","timestamp":1792812593079,"sessionID":"ses_x","part":{"id":"prt_x","reason":"stop","messageID":"msg_x","type":"step-finish","cost":0,"tokens":{"total":45347,"input":45344,"output":3,"reasoning":0,"cache":{"read":0,"write":0}}}}`

	var event mobilecoderEvent
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if event.Type != "step_finish" {
		t.Errorf("type: got %q", event.Type)
	}
	if event.Part.Tokens == nil {
		t.Fatalf("expected tokens block")
	}
	if event.Part.Tokens.Input != 45344 || event.Part.Tokens.Output != 3 {
		t.Errorf("tokens: got input=%d output=%d", event.Part.Tokens.Input, event.Part.Tokens.Output)
	}
	if event.Part.Reason != "stop" {
		t.Errorf("reason: got %q, want stop", event.Part.Reason)
	}
}

// TestMobilecoderUsageIgnoresReasoning pins the measured protocol behaviour:
// mobile reports reasoning as 0 with output already carrying the generation,
// so the accumulator adds neither the reasoning nor the total field.
func TestMobilecoderUsageIgnoresReasoning(t *testing.T) {
	t.Parallel()

	ch := make(chan Message, 16)
	b := &mobilecoderBackend{cfg: Config{Logger: slog.Default()}}
	result := b.processEvents(strings.NewReader(
		`{"type":"step_start","sessionID":"s","part":{"type":"step-start"}}`+"\n"+
			`{"type":"step_finish","sessionID":"s","part":{"type":"step-finish","reason":"stop","tokens":{"total":100,"input":60,"output":40,"reasoning":20,"cache":{"read":5,"write":2}}}}`+"\n"),
		ch)
	if result.usage.InputTokens != 60 || result.usage.OutputTokens != 40 {
		t.Errorf("usage: got input=%d output=%d, want 60/40 (reasoning must not be added)", result.usage.InputTokens, result.usage.OutputTokens)
	}
	if result.usage.CacheReadTokens != 5 || result.usage.CacheWriteTokens != 2 {
		t.Errorf("cache usage: got read=%d write=%d, want 5/2", result.usage.CacheReadTokens, result.usage.CacheWriteTokens)
	}
	if !result.sawTerminalSignal {
		t.Errorf("sawTerminalSignal = false after a step_finish; want true")
	}
}

// TestMobilecoderStreamEndedFailsClosed verifies the terminal-signal guard:
// EOF with a step still open must fail closed, and the error text carries
// the "mobilecoder stream ended" prefix that taskfailure classification
// matches for retry eligibility.
func TestMobilecoderStreamEndedFailsClosed(t *testing.T) {
	t.Parallel()

	ch := make(chan Message, 16)
	b := &mobilecoderBackend{cfg: Config{Logger: slog.Default()}}
	result := b.processEvents(strings.NewReader(
		`{"type":"step_start","sessionID":"s","part":{"type":"step-start"}}`+"\n"),
		ch)
	if result.status != "failed" {
		t.Errorf("status = %q, want failed (step open at EOF)", result.status)
	}
	if !strings.HasPrefix(result.errMsg, "mobilecoder stream ended") {
		t.Errorf("errMsg = %q, want the mobilecoder stream-ended prefix", result.errMsg)
	}
	if result.sawTerminalSignal {
		t.Errorf("sawTerminalSignal = true; want false")
	}
}

// TestMobilecoderEmptyStreamFailsClosed: a stream that never opened a step
// has no positive completion evidence either.
func TestMobilecoderEmptyStreamFailsClosed(t *testing.T) {
	t.Parallel()

	ch := make(chan Message, 16)
	b := &mobilecoderBackend{cfg: Config{Logger: slog.Default()}}
	result := b.processEvents(strings.NewReader(""), ch)
	if result.status != "failed" {
		t.Errorf("status = %q, want failed (no step finished)", result.status)
	}
}

// TestMobilecoderBlockedArgs pins the blocked-arg policy: the daemon-owned
// flags cannot be overridden via CustomArgs, and `--yolo=false` style value
// forms are caught by the standalone mode.
func TestMobilecoderBlockedArgs(t *testing.T) {
	t.Parallel()

	filtered := filterCustomArgs([]string{"--format", "text", "--yolo=false", "--extra"}, mobilecoderBlockedArgs, slog.Default())
	for _, arg := range filtered {
		if arg == "--format" || arg == "text" || strings.HasPrefix(arg, "--yolo") {
			t.Errorf("blocked arg leaked: %v", filtered)
		}
	}
	if !containsString(filtered, "--extra") {
		t.Errorf("non-blocked arg dropped: %v", filtered)
	}
}
