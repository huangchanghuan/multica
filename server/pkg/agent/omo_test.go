package agent

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOmoDescriptor pins the omo (oh-my-openagent) descriptor: a pi-family
// fork that keeps pi's argv contract and session semantics, so it runs the
// standard pi execution path with only identity overrides.
func TestOmoDescriptor(t *testing.T) {
	desc, ok := BuiltinRuntimeByID("omo")
	if !ok {
		t.Fatal("BuiltinRuntimeByID(\"omo\") not found")
	}
	if desc.ProtocolFamily != "pi" {
		t.Errorf("ProtocolFamily = %q, want \"pi\"", desc.ProtocolFamily)
	}
	if desc.DefaultCommand != "omo" || desc.DefaultExecutable != "omo" {
		t.Errorf("command/executable = %q/%q, want \"omo\"/\"omo\"", desc.DefaultCommand, desc.DefaultExecutable)
	}
	if desc.EnvPrefix != "MULTICA_OMO" {
		t.Errorf("EnvPrefix = %q, want \"MULTICA_OMO\"", desc.EnvPrefix)
	}
	if desc.DisplayName != "OmO" {
		t.Errorf("DisplayName = %q, want \"OmO\"", desc.DisplayName)
	}
	if desc.SkillsDir != ".omo/skills" || desc.UserSkillsDir != ".omo/agent/skills" {
		t.Errorf("skills dirs = %q/%q", desc.SkillsDir, desc.UserSkillsDir)
	}
	if !IsBuiltinRuntime("omo") {
		t.Error("IsBuiltinRuntime(\"omo\") = false")
	}
	found := false
	for _, cmd := range BuiltinRuntimeCommands() {
		if cmd == "omo" {
			found = true
		}
	}
	if !found {
		t.Error("BuiltinRuntimeCommands() missing \"omo\"")
	}

	// The backend resolves through the pi family with omo identity overrides.
	b, err := ResolveBackend("omo", Config{Logger: slog.Default()})
	if err != nil {
		t.Fatalf("ResolveBackend(\"omo\"): %v", err)
	}
	pb, ok := b.(*piBackend)
	if !ok {
		t.Fatalf("ResolveBackend(\"omo\") = %T, want *piBackend", b)
	}
	if pb.defaultExecutable != "omo" {
		t.Errorf("defaultExecutable = %q, want \"omo\"", pb.defaultExecutable)
	}
	if pb.providerLabel != "omo" {
		t.Errorf("providerLabel = %q, want \"omo\"", pb.providerLabel)
	}
}

// TestParseOmoModels covers `omo --list-models` table parsing: header skip,
// noise filtering, identity assembly, thinking-column handling, and dedupe.
// Real-catalog shape (verified against omo 5.1.51): provider and model never
// contain spaces, so whitespace splitting is safe.
func TestParseOmoModels(t *testing.T) {
	table := `provider                    model                context  max-out  thinking  images
alibaba-token-plan          glm-5.2              1M       131.1K   yes       no
alibaba-token-plan          deepseek-v4-flash    1M       384K     no        no
9router                     my-combo             128K     16.4K    no        yes
alibaba-token-plan          glm-5.2              1M       131.1K   yes       no
Warning: No models match pattern "stale/model"
Error: unknown flag: --list-models
`
	models := parseOmoModels(table)
	if len(models) != 3 {
		t.Fatalf("len(models) = %d, want 3; got %#v", len(models), models)
	}
	byID := map[string]Model{}
	for _, m := range models {
		byID[m.ID] = m
	}
	glm, ok := byID["alibaba-token-plan/glm-5.2"]
	if !ok {
		t.Fatalf("missing alibaba-token-plan/glm-5.2: %#v", models)
	}
	if glm.Provider != "alibaba-token-plan" {
		t.Errorf("provider = %q", glm.Provider)
	}
	if glm.Thinking == nil {
		t.Fatal("thinking=yes row has nil Thinking")
	}
	got := map[string]bool{}
	for _, level := range glm.Thinking.SupportedLevels {
		got[level.Value] = true
	}
	for _, want := range piThinkingLevelOrder {
		if !got[want] {
			t.Errorf("thinking=yes row missing level %q", want)
		}
	}
	if got["auto"] {
		t.Error("thinking=yes row must not advertise \"auto\"")
	}
	flash, ok := byID["alibaba-token-plan/deepseek-v4-flash"]
	if !ok {
		t.Fatalf("missing alibaba-token-plan/deepseek-v4-flash: %#v", models)
	}
	if flash.Thinking != nil {
		t.Errorf("thinking=no row has Thinking %#v, want nil", flash.Thinking)
	}
	if _, ok := byID["9router/my-combo"]; !ok {
		t.Error("missing 9router/my-combo")
	}
}

// TestParseOmoModelsMalformed pins the structural backstops: rows too short to
// be catalog entries, and stray tokens that cannot form a valid identity, are
// dropped rather than coined into bogus models.
func TestParseOmoModelsMalformed(t *testing.T) {
	models := parseOmoModels("onlyone\n:\n/\nprovider/\n/model\n")
	if len(models) != 0 {
		t.Fatalf("len(models) = %d, want 0; got %#v", len(models), models)
	}
}

// TestOmoThinkingEnum pins the thinking vocabulary: omo inherits pi's fixed
// set verbatim (no `auto`), and the thinking UI is enabled for it.
func TestOmoThinkingEnum(t *testing.T) {
	for _, level := range []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"} {
		if !IsKnownThinkingValue("omo", level) {
			t.Errorf("IsKnownThinkingValue(\"omo\", %q) = false", level)
		}
	}
	if IsKnownThinkingValue("omo", "auto") {
		t.Error("IsKnownThinkingValue(\"omo\", \"auto\") = true, want false")
	}
	if !ThinkingControlSupported("omo") {
		t.Error("ThinkingControlSupported(\"omo\") = false, want true")
	}
}

// TestOmoMcpFailFast pins the phase-1 MCP boundary: omo never receives the
// generated pi extension, and a managed MCP config is an explicit runtime
// error instead of a silent no-op.
func TestOmoMcpFailFast(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	backend := &piBackend{providerLabel: "omo"}

	for _, empty := range []json.RawMessage{
		nil,
		json.RawMessage("null"),
		json.RawMessage(`{}`),
		json.RawMessage(`{"mcpServers":{}}`),
	} {
		path, err := backend.preparePiMcpExtension(empty)
		if err != nil || path != "" {
			t.Fatalf("empty config %s = (%q, %v), want no extension and no error", empty, path, err)
		}
	}

	raw := json.RawMessage(`{"mcpServers":{"probe":{"command":"echo"}}}`)
	path, err := backend.preparePiMcpExtension(raw)
	if err == nil {
		removePiMcpExtension(path)
		t.Fatal("managed MCP config accepted for omo, want fail-fast error")
	}
	if !strings.Contains(err.Error(), "omo does not support managed MCP") {
		t.Fatalf("error %q missing the omo MCP explanation", err)
	}
	matches, globErr := filepath.Glob(filepath.Join(os.Getenv("TMPDIR"), piMcpExtensionDirPrefix+"*"))
	if globErr != nil {
		t.Fatal(globErr)
	}
	if len(matches) != 0 {
		t.Fatalf("omo MCP rejection leaked extension directories: %v", matches)
	}
}

// TestOmoConfiguredProvidersIn pins the filter-set semantics: the union of
// models.json `providers` keys (custom endpoints) and auth.json top-level
// keys (stored credentials). readable is false only when neither file exists.
func TestOmoConfiguredProvidersIn(t *testing.T) {
	t.Run("union of models.json and auth.json", func(t *testing.T) {
		dir := t.TempDir()
		write := func(name, content string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		write("models.json", `{"providers":{"magpie":{"baseUrl":"http://127.0.0.1:3425/v1"},"9router":{}}}`)
		write("auth.json", `{"magpie":{"type":"api"},"openrouter":{"key":"k"}}`)
		set, readable := omoConfiguredProvidersIn(dir)
		if !readable {
			t.Fatal("readable = false, want true")
		}
		for _, id := range []string{"magpie", "9router", "openrouter"} {
			if !set[id] {
				t.Errorf("provider %q missing from filter set %v", id, set)
			}
		}
		if len(set) != 3 {
			t.Errorf("set = %v, want exactly 3 providers", set)
		}
	})

	t.Run("neither file readable", func(t *testing.T) {
		set, readable := omoConfiguredProvidersIn(t.TempDir())
		if readable {
			t.Error("readable = true for empty dir, want false")
		}
		if len(set) != 0 {
			t.Errorf("set = %v, want empty", set)
		}
	})

	t.Run("malformed json still counts as read", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, readable := omoConfiguredProvidersIn(dir)
		if !readable {
			t.Error("readable = false for malformed models.json, want true (file existed)")
		}
	})
}

// TestOmoFilterConfigured narrows the parsed catalog: only rows whose
// Provider is configured survive; an unreadable config dir keeps everything
// (the filter must not zero out a working discovery).
func TestOmoFilterConfigured(t *testing.T) {
	catalog := parseOmoModels(`provider              model                context  max-out  thinking  images
magpie                auto/glm-5.3         1M       64K      yes       yes
magpie                workbuddy-ai/glm-5.3 1M       64K      yes       yes
openrouter            openai/gpt-4o        128K     16.4K    yes       yes
9router               my-combo             128K     16.4K    no        yes
`)
	if len(catalog) != 4 {
		t.Fatalf("catalog = %d models, want 4", len(catalog))
	}

	t.Run("keeps only configured providers", func(t *testing.T) {
		dir := t.TempDir()
		modelsJSON := `{"providers":{"magpie":{},"9router":{}}}`
		authJSON := `{"magpie":{}}`
		if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte(modelsJSON), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(authJSON), 0o600); err != nil {
			t.Fatal(err)
		}
		filtered := omoFilterConfiguredIn(dir, catalog)
		if len(filtered) != 3 {
			t.Fatalf("filtered = %d models, want 3 (2 magpie + 1 9router)", len(filtered))
		}
		for _, m := range filtered {
			if m.Provider == "openrouter" {
				t.Errorf("unconfigured provider %q leaked into filtered list", m.Provider)
			}
		}
		if filtered[0].ID != "magpie/auto/glm-5.3" || filtered[0].Thinking == nil {
			t.Errorf("first = %+v, want magpie/auto/glm-5.3 with thinking", filtered[0])
		}
	})

	t.Run("unreadable config dir degrades to full catalog", func(t *testing.T) {
		filtered := omoFilterConfiguredIn(t.TempDir(), catalog)
		if len(filtered) != len(catalog) {
			t.Errorf("filtered = %d, want full %d when config unreadable", len(filtered), len(catalog))
		}
	})

	t.Run("readable but empty config yields empty list", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte(`{"providers":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		filtered := omoFilterConfiguredIn(dir, catalog)
		if len(filtered) != 0 {
			t.Errorf("filtered = %d, want 0 when nothing configured", len(filtered))
		}
	})
}
