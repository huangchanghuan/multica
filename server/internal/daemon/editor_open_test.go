package daemon

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/util"
)

// editorTestDaemon wires a Daemon with a temp WorkspacesRoot, a stub editor
// detection (so tests don't depend on the build host's installed CLIs), and
// nothing else — the editor handlers only touch cfg.WorkspacesRoot, logger,
// and the editorOpen cache.
func editorTestDaemon(t *testing.T, workspaceRoot string) *Daemon {
	t.Helper()
	d := &Daemon{logger: slog.Default()}
	d.cfg.WorkspacesRoot = workspaceRoot
	d.editorOpen = &editorOpenState{
		now: func() time.Time { return time.Now() },
		detect: func() ([]detectedEditor, []string) {
			return []detectedEditor{
				{definition: editorTable[0], command: "/bin/true"}, // Trae, no baseArg
			}, nil
		},
	}
	return d
}

// seedWorkdir creates the {root}/{workspace}/{task}/workdir layout with a few
// entries and returns the workdir path.
func seedWorkdir(t *testing.T) (root, workdir string) {
	t.Helper()
	root = t.TempDir()
	workdir = filepath.Join(root, "ws1", "task1", "workdir")
	if err := os.MkdirAll(filepath.Join(workdir, "output"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workdir, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"CLAUDE.md", "README.md", ".env"} {
		if err := os.WriteFile(filepath.Join(workdir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root, workdir
}

// editorTestRequest builds a request the way the browser sends it: Host is
// the loopback health server. httptest.NewRequest would otherwise default to
// example.com and fail the Host allowlist before reaching the handler.
func editorTestRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Host = "127.0.0.1:19514"
	return req
}

func TestEditorLoopbackHost(t *testing.T) {
	cases := []struct {
		host string
		ok   bool
	}{
		{"127.0.0.1:19514", true},
		{"localhost:19514", true},
		{"[::1]:19514", true},
		{"127.0.0.1", true},
		{"", false},
		{"evil.example:19514", false},
		{"127.0.0.1.evil.com:19514", false},
		{"[::1]:19514.evil.com", false},
		{"192.168.1.5:19514", false},
	}
	for _, tc := range cases {
		if got := isLoopbackHost(tc.host); got != tc.ok {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", tc.host, got, tc.ok)
		}
	}
}

func TestEditorValidateEntryName(t *testing.T) {
	valid := []string{"", "CLAUDE.md", ".env", "output", "a b.txt", "中文.md", "a..b"}
	for _, name := range valid {
		if err := validateEntryName(name); err != nil {
			t.Errorf("validateEntryName(%q) = %v, want nil", name, err)
		}
	}
	invalid := []string{
		"..", ".", "a/b", "a\\b", "-flag", "x\ny", "x\ty",
		"CON", "nul.txt", "com1", "LPT2", "x/", "/abs",
	}
	for _, name := range invalid {
		if err := validateEntryName(name); err == nil {
			t.Errorf("validateEntryName(%q) = nil, want error", name)
		}
	}
}

func TestEditorValidateWorkdirPath(t *testing.T) {
	root, workdir := seedWorkdir(t)
	d := editorTestDaemon(t, root)

	if err := d.validateWorkdirPath(workdir); err != nil {
		t.Fatalf("validateWorkdirPath(workdir) = %v, want nil", err)
	}
	if err := d.validateWorkdirPath(filepath.Join(workdir, "output")); err != nil {
		t.Fatalf("validateWorkdirPath(subdir) = %v, want nil", err)
	}

	// A path outside the root.
	if err := d.validateWorkdirPath(t.TempDir()); err == nil {
		t.Error("validateWorkdirPath(outside root) = nil, want error")
	}
	// A symlink inside the workdir pointing outside the root: the workdir
	// itself is fine, the link target must be rejected.
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(workdir, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := d.validateWorkdirPath(filepath.Join(workdir, "escape")); err == nil {
		t.Error("validateWorkdirPath(symlink escape) = nil, want error")
	}
	// A trailing separator must not change the containment answer.
	if err := d.validateWorkdirPath(workdir + string(filepath.Separator)); err != nil {
		t.Fatalf("validateWorkdirPath(trailing sep) = %v, want nil", err)
	}
}

func TestEditorResolveOpenTargetRejectsSymlinkEscape(t *testing.T) {
	root, workdir := seedWorkdir(t)
	d := editorTestDaemon(t, root)

	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(workdir, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := d.resolveOpenTarget(workdir, "escape"); err == nil {
		t.Error("resolveOpenTarget(entry=symlink escape) = nil, want error")
	}
	if _, err := d.resolveOpenTarget(workdir, ""); err != nil {
		t.Fatalf("resolveOpenTarget(entry=\"\") = %v, want nil", err)
	}
	if _, err := d.resolveOpenTarget(workdir, "CLAUDE.md"); err != nil {
		t.Fatalf("resolveOpenTarget(entry=CLAUDE.md) = %v, want nil", err)
	}
}

func TestEditorListWorkdirEntries(t *testing.T) {
	_, workdir := seedWorkdir(t)
	entries, err := listWorkdirEntries(workdir)
	if err != nil {
		t.Fatal(err)
	}
	// Directories first, both groups case-insensitive alpha; dotfiles included.
	want := []editorEntry{
		{Name: ".agent", IsDir: true},
		{Name: "output", IsDir: true},
		{Name: ".env", IsDir: false},
		{Name: "CLAUDE.md", IsDir: false},
		{Name: "README.md", IsDir: false},
	}
	if len(entries) != len(want) {
		t.Fatalf("entries = %#v, want %#v", entries, want)
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Errorf("entries[%d] = %#v, want %#v", i, entries[i], want[i])
		}
	}
}

func TestEditorOptionsHandler(t *testing.T) {
	root, workdir := seedWorkdir(t)
	d := editorTestDaemon(t, root)

	req := editorTestRequest(http.MethodGet, "/editor/options?path="+workdir, nil)
	rec := httptest.NewRecorder()
	d.editorOptionsHandler()(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp editorOptionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Editors) != 1 || resp.Editors[0].ID != "trae" {
		t.Fatalf("editors = %#v", resp.Editors)
	}
	if len(resp.Entries) != 5 || !resp.Entries[0].IsDir {
		t.Fatalf("entries = %#v", resp.Entries)
	}

	// Path outside the root → 403.
	req = editorTestRequest(http.MethodGet, "/editor/options?path="+t.TempDir(), nil)
	rec = httptest.NewRecorder()
	d.editorOptionsHandler()(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("outside-root status = %d", rec.Code)
	}

	// POST on options (non-OPTIONS mismatch) → 405.
	req = editorTestRequest(http.MethodPost, "/editor/options?path="+workdir, nil)
	rec = httptest.NewRecorder()
	d.editorOptionsHandler()(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST options status = %d", rec.Code)
	}

	// OPTIONS preflight passes through the method gate as 204.
	req = editorTestRequest(http.MethodOptions, "/editor/options", nil)
	rec = httptest.NewRecorder()
	d.editorOptionsHandler()(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d", rec.Code)
	}
}

func TestEditorPreflight(t *testing.T) {
	d := editorTestDaemon(t, t.TempDir())
	handler := d.editorOptionsHandler()

	req := editorTestRequest(http.MethodOptions, "/editor/options", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	req.Header.Set("Access-Control-Request-Private-Network", "true")
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d", rec.Code)
	}
	h := rec.Header()
	if h.Get("Access-Control-Allow-Origin") != "https://app.example.com" {
		t.Errorf("ACAO = %q", h.Get("Access-Control-Allow-Origin"))
	}
	if h.Get("Access-Control-Allow-Private-Network") != "true" {
		t.Errorf("ACA-PN = %q", h.Get("Access-Control-Allow-Private-Network"))
	}
	if h.Get("Access-Control-Allow-Headers") == "" {
		t.Error("ACAH missing")
	}

	// No PNA request header → no PNA response header.
	req = editorTestRequest(http.MethodOptions, "/editor/options", nil)
	req.Header.Set("Origin", "https://app.example.com")
	rec = httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight(2) status = %d", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Private-Network") != "" {
		t.Error("ACA-PN set without request header")
	}

	// Spoofed Host → 403 before anything else.
	req = httptest.NewRequest(http.MethodOptions, "/editor/options", nil)
	req.Host = "evil.example:19514"
	rec = httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("rebinding status = %d", rec.Code)
	}
}

func TestEditorOpenHandler(t *testing.T) {
	root, workdir := seedWorkdir(t)
	d := editorTestDaemon(t, root)
	handler := d.editorOpenHandler()
	openBody := func(s string) *http.Request {
		req := editorTestRequest(http.MethodPost, "/editor/open", strings.NewReader(s))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(editorLoopbackHeader, "1")
		return req
	}

	// Missing loopback marker → 403.
	req := editorTestRequest(http.MethodPost, "/editor/open",
		strings.NewReader(`{"path":"`+workdir+`","editor":"trae"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("no-marker status = %d", rec.Code)
	}

	// Unknown editor id → 400.
	rec = httptest.NewRecorder()
	handler(rec, openBody(`{"path":"`+workdir+`","editor":"nonexistent"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown editor status = %d", rec.Code)
	}

	// Entry that fails the name rule → 400.
	rec = httptest.NewRecorder()
	handler(rec, openBody(`{"path":"`+workdir+`","entry":"../escape","editor":"trae"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad entry status = %d", rec.Code)
	}

	// Path outside the root → 403.
	rec = httptest.NewRecorder()
	handler(rec, openBody(`{"path":"`+t.TempDir()+`","editor":"trae"}`))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("outside-root status = %d", rec.Code)
	}

	// Happy path: /bin/true swallows any argv.
	req = openBody(`{"path":"`+workdir+`","entry":"CLAUDE.md","editor":"trae"}`)
	req.Header.Set("Origin", "https://app.example.com")
	rec = httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("open status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["status"] != "launched" || resp["editor"] != "Trae" {
		t.Fatalf("resp = %#v", resp)
	}
	if resp["target"] == "" || filepath.Base(resp["target"]) != "CLAUDE.md" {
		t.Fatalf("target = %q", resp["target"])
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "https://app.example.com" {
		t.Errorf("ACAO = %q", rec.Header().Get("Access-Control-Allow-Origin"))
	}
}

func TestEditorProbeCacheTTL(t *testing.T) {
	d := &Daemon{logger: slog.Default()}
	calls := 0
	now := time.Now()
	st := &editorOpenState{
		now: func() time.Time { return now },
		detect: func() ([]detectedEditor, []string) {
			calls++
			return []detectedEditor{{definition: editorTable[0], command: "/bin/true"}}, nil
		},
	}
	d.editorOpen = st

	for i := 0; i < 5; i++ {
		editors, _ := st.snapshot()
		if len(editors) != 1 {
			t.Fatalf("editors = %#v", editors)
		}
	}
	if calls != 1 {
		t.Fatalf("detect calls = %d, want 1 (cache should hold within TTL)", calls)
	}
	now = now.Add(2 * editorProbeCacheTTL)
	st.snapshot()
	st.snapshot()
	if calls != 2 {
		t.Fatalf("detect calls = %d, want 2 (TTL expired)", calls)
	}
}

func TestEditorRequireInsideRoot(t *testing.T) {
	root := string(filepath.Separator) + "tmp" + string(filepath.Separator) + "ws"
	if err := requireInsideRoot(root, root); err != nil {
		t.Errorf("root itself rejected: %v", err)
	}
	if err := requireInsideRoot(root, filepath.Join(root, "a", "b")); err != nil {
		t.Errorf("descendant rejected: %v", err)
	}
	sibling := string(filepath.Separator) + "tmp" + string(filepath.Separator) + "ws-other"
	if err := requireInsideRoot(root, sibling); err == nil {
		t.Error("sibling prefix path accepted")
	}
	outside := string(filepath.Separator) + "etc"
	if err := requireInsideRoot(root, outside); err == nil {
		t.Error("outside path accepted")
	}
}

// Guard against regression on the #8946 Windows lesson: the plan mandates
// util.ResolveSymlinks (junction-safe), and the tests above exercise its Unix
// behaviour. This test pins that a symlink-spelled workdir inside the root
// resolves to the same containment decision as the direct spelling.
func TestEditorRootSpellingEquivalence(t *testing.T) {
	root, workdir := seedWorkdir(t)
	if err := os.Symlink(workdir, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	d := editorTestDaemon(t, root)
	resolved, err := util.ResolveSymlinks(filepath.Join(root, "link"))
	if err != nil {
		t.Fatal(err)
	}
	if resolved != workdir {
		t.Fatalf("ResolveSymlinks(link) = %q, want %q", resolved, workdir)
	}
	if err := d.validateWorkdirPath(filepath.Join(root, "link")); err != nil {
		t.Fatalf("validateWorkdirPath(link spelling) = %v, want nil", err)
	}
}
