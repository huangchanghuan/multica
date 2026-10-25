//go:build linux

package daemon

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// editorSpawnSysProcAttr detaches the editor from the daemon's process group
// (session leader via Setsid) so closing a terminal or killing the daemon's
// process group does not take the editor down. Note the cgroup boundary:
// `systemctl --user stop multica-daemon` still reaps the child — documented
// limitation, not a bug.
func editorSpawnSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// graphicalEnvKeys are the variables a GUI editor needs that a systemd user
// service may lack. XDG_RUNTIME_DIR and DBUS_SESSION_BUS_ADDRESS are usually
// present (injected when the user manager starts), but they are collected
// too when missing so the spawned editor gets a complete session view.
var graphicalEnvKeys = []string{
	"DISPLAY", "WAYLAND_DISPLAY", "XAUTHORITY", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS",
}

// graphicalEnvCandidates ranks process names whose environments most likely
// describe the user's interactive graphical session: the compositor, the
// display server, and desktop shell processes.
var graphicalEnvCandidates = []string{
	"gnome-shell", "plasmashell", "kwin_wayland", "kwin_x11", "Xorg", "Xwayland",
	"deepin-wm", "dde-desktop", "dde-shell", "weston", "sway", "labwc", "hyprland", "mutter",
}

// graphicalEnvExtras returns "K=V" pairs to append to the editor's
// environment when the daemon itself runs headless (typical for a systemd
// user service started before login). It walks /proc for a same-uid process
// whose environment carries a graphical session — preferring the compositor
// or display server — and copies the missing variables from there.
func graphicalEnvExtras() []string {
	if os.Getenv("WAYLAND_DISPLAY") != "" || os.Getenv("DISPLAY") != "" {
		return nil
	}
	proc, err := os.Open("/proc")
	if err != nil {
		return nil
	}
	defer proc.Close()
	names, err := proc.Readdirnames(-1)
	if err != nil {
		return nil
	}
	uid := strconv.Itoa(os.Getuid())
	_ = uid // /proc/<pid>/environ is readable only for same-uid processes (or root); the daemon runs unprivileged, so a successful read is the ownership check itself.

	// Two passes: prefer the known compositor/display-server names, then
	// accept any same-uid process that has a session variable (the user may
	// be on an unknown desktop).
	for _, preferKnown := range []bool{true, false} {
		var best []string
		var bestRank int
		for _, name := range names {
			pid, err := strconv.Atoi(name)
			if err != nil || pid <= 0 {
				continue
			}
			env := readProcEnv(pid)
			if len(env) == 0 {
				continue
			}
			if env["DISPLAY"] == "" && env["WAYLAND_DISPLAY"] == "" {
				continue
			}
			comm := strings.TrimSpace(readProcComm(pid))
			rank := 0
			if preferKnown {
				if !isKnownGraphicalProcess(comm) {
					continue
				}
				rank = 1
			} else if isKnownGraphicalProcess(comm) {
				rank = 2
			}
			if best == nil || rank > bestRank {
				extras := missingGraphicalVars(env)
				if len(extras) > 0 {
					best = extras
					bestRank = rank
				}
			}
		}
		if best != nil {
			return best
		}
	}
	return nil
}

func isKnownGraphicalProcess(comm string) bool {
	for _, candidate := range graphicalEnvCandidates {
		if comm == candidate {
			return true
		}
	}
	return false
}

// missingGraphicalVars copies only the variables the daemon does not already
// have, in graphicalEnvKeys order.
func missingGraphicalVars(env map[string]string) []string {
	var extras []string
	for _, key := range graphicalEnvKeys {
		if os.Getenv(key) != "" {
			continue
		}
		if v, ok := env[key]; ok && v != "" {
			extras = append(extras, key+"="+v)
		}
	}
	return extras
}

func readProcEnv(pid int) map[string]string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return nil
	}
	env := make(map[string]string)
	for _, entry := range bytes.Split(data, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		k, v, ok := strings.Cut(string(entry), "=")
		if ok {
			env[k] = v
		}
	}
	return env
}

func readProcComm(pid int) string {
	f, err := os.Open(filepath.Join("/proc", strconv.Itoa(pid), "comm"))
	if err != nil {
		return ""
	}
	defer f.Close()
	line, _ := bufio.NewReader(f).ReadString('\n')
	return strings.TrimRight(line, "\n")
}
