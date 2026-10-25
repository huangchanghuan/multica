// Loopback port of the daemon's health server (server/internal/daemon/config.go
// DefaultHealthPort). The web UI talks to it directly for browser-driven local
// actions ("open in editor"); the daemon does not report the port to the
// server, so a non-default port (named daemon profile, port fallback) simply
// makes these features degrade to hidden — see EDITOR_OPEN_PLAN.md §8.
export const DAEMON_HEALTH_PORT = 19514;
