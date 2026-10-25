package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/multica-ai/multica/server/internal/util"
)

// "Open in editor" lets the browser-driven web UI launch a local editor on
// the daemon machine. The web page fetches the daemon's loopback health
// server directly (design doc EDITOR_OPEN_PLAN.md): the machine that renders
// the button is the machine whose editor would open, so no server relay is
// needed. Two endpoints hang off the health mux:
//
//	GET  /editor/options?path=<abs task workdir>  → editors + workdir entries
//	POST /editor/open {path, entry?, editor}      → spawn the editor
//
// Security model (see the plan's §6): the health server is an unauthenticated
// loopback service, so these endpoints lean on browser-enforced boundaries —
// Host allowlist (DNS rebinding), CORS/PNA preflight (drive-by) — plus
// server-side path containment inside WorkspacesRoot. There is no auth token:
// /repo/checkout's bearer tokens are bound to agent processes, which a
// browser can never hold.

const editorProbeCacheTTL = 5 * time.Minute

// editorDefinition is one entry of the built-in editor table. Command is
// never taken from the request: the wire format carries only the ID, and the
// daemon looks the executable up in its own table.
type editorDefinition struct {
	id      string
	label   string
	probes  []string // CLI names tried in order via exec.LookPath
	baseArg []string // fixed arguments prepended to the target path
}

var editorTable = []editorDefinition{
	{id: "trae", label: "Trae", probes: []string{"trae-cn", "trae"}},
	{id: "vscode", label: "VS Code", probes: []string{"code"}, baseArg: []string{"--new-window"}},
	{id: "cursor", label: "Cursor", probes: []string{"cursor"}, baseArg: []string{"--new-window"}},
	{id: "windsurf", label: "Windsurf", probes: []string{"windsurf"}, baseArg: []string{"--new-window"}},
	{id: "zed", label: "Zed", probes: []string{"zed"}},
	{id: "vscodium", label: "VSCodium", probes: []string{"codium"}, baseArg: []string{"--new-window"}},
	{id: "idea", label: "IntelliJ IDEA", probes: []string{"idea"}},
	{id: "pycharm", label: "PyCharm", probes: []string{"pycharm"}},
	{id: "clion", label: "CLion", probes: []string{"clion"}},
	{id: "goland", label: "GoLand", probes: []string{"goland"}},
	{id: "webstorm", label: "WebStorm", probes: []string{"webstorm"}},
	{id: "phpstorm", label: "PhpStorm", probes: []string{"phpstorm"}},
	{id: "rider", label: "Rider", probes: []string{"rider"}},
	{id: "datagrip", label: "DataGrip", probes: []string{"datagrip"}},
	{id: "rubymine", label: "RubyMine", probes: []string{"rubymine"}},
	{id: "studio", label: "Android Studio", probes: []string{"studio"}},
	{id: "qoder", label: "Qoder", probes: []string{"qoder"}},
	{id: "codebuddy", label: "CodeBuddy", probes: []string{"codebuddy"}},
}

// detectedEditor is a definition whose CLI was found on PATH.
type detectedEditor struct {
	definition editorDefinition
	command    string // absolute path from exec.LookPath
}

// editorOpenState caches the PATH probe (and, on Linux, the resolved
// graphical session environment) so repeated issue-card opens never re-scan.
type editorOpenState struct {
	mu        sync.Mutex
	editors   []detectedEditor
	envExtras []string // Linux graphical session variables, "K=V" form
	expiresAt time.Time
	// now and detect are swappable in tests.
	now    func() time.Time
	detect func() ([]detectedEditor, []string)
}

func (d *Daemon) editorState() *editorOpenState {
	if d.editorOpen == nil {
		d.editorOpen = &editorOpenState{
			now:    time.Now,
			detect: detectEditorsAndGraphicalEnv,
		}
	}
	return d.editorOpen
}

// snapshot returns the cached editor list (and env extras), refreshing it
// after the TTL lapses.
func (s *editorOpenState) snapshot() (editors []detectedEditor, envExtras []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.editors != nil && s.now().Before(s.expiresAt) {
		return s.editors, s.envExtras
	}
	editors, envExtras = s.detect()
	s.editors = editors
	s.envExtras = envExtras
	s.expiresAt = s.now().Add(editorProbeCacheTTL)
	return editors, envExtras
}

func detectEditorsAndGraphicalEnv() ([]detectedEditor, []string) {
	var found []detectedEditor
	for _, def := range editorTable {
		for _, probe := range def.probes {
			if path, err := exec.LookPath(probe); err == nil {
				found = append(found, detectedEditor{definition: def, command: path})
				break
			}
		}
	}
	return found, graphicalEnvExtras()
}

// editorOptionsResponse is the body of GET /editor/options.
type editorOptionsResponse struct {
	Editors []editorOption `json:"editors"`
	Entries []editorEntry  `json:"entries"`
}

type editorOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type editorEntry struct {
	Name string `json:"name"`
	IsDir bool  `json:"is_dir"`
}

// editorOpenRequest is the body of POST /editor/open.
type editorOpenRequest struct {
	Path   string `json:"path"`
	Entry  string `json:"entry,omitempty"`
	Editor string `json:"editor"`
}

const editorLoopbackHeader = "X-Multica-Web"

// editorOptionsHandler serves GET /editor/options.
func (d *Daemon) editorOptionsHandler() http.HandlerFunc {
	return d.editorGuardHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		workdir := r.URL.Query().Get("path")
		if workdir == "" {
			http.Error(w, "path query parameter is required", http.StatusBadRequest)
			return
		}
		if err := d.validateWorkdirPath(workdir); err != nil {
			d.logger.Warn("editor options rejected", "path", workdir, "error", err)
			http.Error(w, "path is not an accepted workdir", http.StatusForbidden)
			return
		}
		entries, err := listWorkdirEntries(workdir)
		if err != nil {
			d.logger.Warn("editor options listing failed", "path", workdir, "error", err)
			http.Error(w, "workdir is not readable", http.StatusBadRequest)
			return
		}
		editors, _ := d.editorState().snapshot()
		resp := editorOptionsResponse{
			Editors: make([]editorOption, 0, len(editors)),
			Entries: entries,
		}
		for _, e := range editors {
			resp.Editors = append(resp.Editors, editorOption{ID: e.definition.id, Label: e.definition.label})
		}
		if resp.Entries == nil {
			resp.Entries = []editorEntry{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})
}

// editorOpenHandler serves POST /editor/open.
func (d *Daemon) editorOpenHandler() http.HandlerFunc {
	return d.editorGuardHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// A plain HTML form can carry only simple request headers, so a
		// required custom header is another speed bump for drive-by calls
		// (browsers that skip preflight still cannot set it without CORS).
		if r.Header.Get(editorLoopbackHeader) != "1" {
			http.Error(w, "missing loopback marker", http.StatusForbidden)
			return
		}
		var req editorOpenRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		if req.Path == "" {
			http.Error(w, "path is required", http.StatusBadRequest)
			return
		}
		if err := validateEntryName(req.Entry); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		editors, envExtras := d.editorState().snapshot()
		var chosen *detectedEditor
		for i := range editors {
			if editors[i].definition.id == req.Editor {
				chosen = &editors[i]
				break
			}
		}
		if chosen == nil {
			http.Error(w, "editor is not available on this machine", http.StatusBadRequest)
			return
		}
		target, err := d.resolveOpenTarget(req.Path, req.Entry)
		if err != nil {
			d.logger.Warn("editor open rejected",
				"path", req.Path, "entry", req.Entry, "editor", req.Editor, "error", err)
			http.Error(w, "path is not an accepted workdir", http.StatusForbidden)
			return
		}
		if err := spawnEditor(chosen, target, envExtras); err != nil {
			d.logger.Warn("editor spawn failed", "editor", chosen.definition.id, "target", target, "error", err)
			http.Error(w, "failed to launch editor", http.StatusInternalServerError)
			return
		}
		d.logger.Info("editor opened", "editor", chosen.definition.id, "target", target)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status": "launched",
			"editor": chosen.definition.label,
			"target": target,
		})
	})
}

// editorGuardHandler wraps both editor endpoints with the loopback-specific
// defenses: Host allowlist first (the only line that holds against DNS
// rebinding, where the request looks same-origin to the browser), then CORS
// and Private Network Access negotiation. OPTIONS is answered before the
// per-endpoint method check because the preflight for GET /editor/options and
// POST /editor/open alike arrives as OPTIONS.
func (d *Daemon) editorGuardHandler(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackHost(r.Host) {
			http.Error(w, "loopback only", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodOptions {
			writeEditorPreflight(w, r)
			return
		}
		// Cross-origin reads need an explicit opt-in; same-origin callers
		// (curl, the desktop app) send no Origin and need nothing.
		if origin := r.Header.Get("Origin"); origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
		}
		next(w, r)
	}
}

func writeEditorPreflight(w http.ResponseWriter, r *http.Request) {
	if origin := r.Header.Get("Origin"); origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
	}
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, "+editorLoopbackHeader)
	w.Header().Set("Access-Control-Max-Age", "600")
	// Chrome sends this on every public-page → loopback request and blocks
	// the follow-up unless the server opts in. Safari/Firefox are covered by
	// the degrade path (options fetch fails → section hidden).
	if r.Header.Get("Access-Control-Request-Private-Network") == "true" {
		w.Header().Set("Access-Control-Allow-Private-Network", "true")
	}
	w.Header().Add("Vary", "Origin")
	w.Header().Add("Vary", "Access-Control-Request-Method")
	w.Header().Add("Vary", "Access-Control-Request-Headers")
	w.WriteHeader(http.StatusNoContent)
}

// isLoopbackHost validates the Host header against the loopback names. A
// spoofed DNS name pointing at 127.0.0.1 makes the request same-origin in the
// browser, defeating CORS and PNA; the Host check is the only guard left.
// The port part, when present, must be numeric — "[::1]:19514.evil.com" style
// spellings must not slip through as a bare IPv6 name.
func isLoopbackHost(host string) bool {
	if host == "" {
		return false
	}
	var name string
	if rest, ok := strings.CutPrefix(host, "["); ok {
		name, rest, found := strings.Cut(rest, "]")
		if !found {
			return false
		}
		if rest != "" && (rest[0] != ':' || !isASCIIDigits(rest[1:])) {
			return false
		}
		return loopbackName(name)
	}
	if i := strings.LastIndex(host, ":"); i >= 0 {
		if !isASCIIDigits(host[i+1:]) {
			return false
		}
		name = host[:i]
	} else {
		name = host
	}
	return loopbackName(name)
}

func loopbackName(name string) bool {
	switch strings.ToLower(strings.Trim(name, "[]")) {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

func isASCIIDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// validateWorkdirPath canonicalizes the client-supplied workdir and requires
// it to live inside WorkspacesRoot. The client only ever sends paths it read
// from the server, but the daemon re-derives the trust: only paths the
// containment check passes are listed or opened.
func (d *Daemon) validateWorkdirPath(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("empty path")
	}
	root, err := util.ResolveSymlinks(d.cfg.WorkspacesRoot)
	if err != nil {
		return fmt.Errorf("resolve workspaces root: %w", err)
	}
	resolved, err := util.ResolveSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve workdir: %w", err)
	}
	return requireInsideRoot(root, resolved)
}

// resolveOpenTarget validates the workdir and the joined entry, mirroring the
// plan's "prefix strict, tail best-effort" rule: the entry may have vanished
// between listing and opening (TOCTOU), so only the tail is resolved leniently.
func (d *Daemon) resolveOpenTarget(path, entry string) (string, error) {
	if err := d.validateWorkdirPath(path); err != nil {
		return "", err
	}
	root, err := util.ResolveSymlinks(d.cfg.WorkspacesRoot)
	if err != nil {
		return "", fmt.Errorf("resolve workspaces root: %w", err)
	}
	target := path
	if entry != "" {
		target = filepath.Join(path, entry)
	}
	resolved, err := util.ResolveSymlinksBestEffort(target)
	if err != nil {
		return "", fmt.Errorf("resolve target: %w", err)
	}
	if err := requireInsideRoot(root, resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

// requireInsideRoot reports whether resolved is the root itself or a
// descendant. Both inputs must already be canonical.
func requireInsideRoot(root, resolved string) error {
	if resolved == root {
		return nil
	}
	if !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
		return fmt.Errorf("path %q escapes workspaces root %q", resolved, root)
	}
	return nil
}

// validateEntryName enforces the bare-filename rule. Empty means "open the
// workdir itself" and is valid.
func validateEntryName(entry string) error {
	if entry == "" {
		return nil
	}
	if entry != filepath.Base(entry) {
		return errors.New("entry must be a bare file name")
	}
	if entry == "." || entry == ".." {
		return errors.New("entry must not be a relative path segment")
	}
	if strings.HasPrefix(entry, "-") {
		return errors.New("entry must not start with '-'")
	}
	if strings.Contains(entry, "\\") {
		return errors.New("entry must not contain a path separator")
	}
	for _, r := range entry {
		if r < 0x20 || r == 0x7f {
			return errors.New("entry must not contain control characters")
		}
	}
	if isWindowsReservedName(entry) {
		return errors.New("entry uses a reserved device name")
	}
	return nil
}

// isWindowsReservedName blocks the DOS device names. Applied on every
// platform: the check is cheap, names are rare, and an entry that only
// misbehaves on Windows would otherwise need a platform-specific rule.
func isWindowsReservedName(name string) bool {
	base := strings.ToUpper(strings.SplitN(name, ".", 2)[0])
	switch base {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(base) == 4 && strings.HasPrefix(base, "COM") && isDigitByte(base[3]) {
		return true
	}
	if len(base) == 4 && strings.HasPrefix(base, "LPT") && isDigitByte(base[3]) {
		return true
	}
	return false
}

func isDigitByte(b byte) bool { return b >= '0' && b <= '9' }

// listWorkdirEntries returns the direct children of the workdir, directories
// first, both groups name-sorted (case-insensitive). Dotfiles are included:
// agents routinely create .env, .cursorrules and friends, and the copy-path
// affordance already exposes the same names.
func listWorkdirEntries(workdir string) ([]editorEntry, error) {
	dirEntries, err := os.ReadDir(workdir)
	if err != nil {
		return nil, err
	}
	entries := make([]editorEntry, 0, len(dirEntries))
	for _, de := range dirEntries {
		entries = append(entries, editorEntry{Name: de.Name(), IsDir: de.IsDir()})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	return entries, nil
}

// spawnEditor launches the editor detached so it outlives the request (and,
// short of a cgroup teardown, the daemon itself). The child environment is
// the daemon's own plus, on Linux, the graphical session variables recovered
// by resolveGraphicalEnv — a systemd user service has no DISPLAY of its own.
func spawnEditor(editor *detectedEditor, target string, envExtras []string) error {
	args := append(append([]string{}, editor.definition.baseArg...), target)
	cmd := exec.Command(editor.command, args...)
	cmd.Env = append(os.Environ(), envExtras...)
	cmd.SysProcAttr = editorSpawnSysProcAttr()
	if err := cmd.Start(); err != nil {
		return err
	}
	// Detached on purpose: the daemon does not wait inline, but it must reap
	// the child — without a Wait(), every launch leaves a zombie in the
	// daemon's process table (observed: [code] <defunct>).
	go func() { _ = cmd.Wait() }()
	return nil
}
