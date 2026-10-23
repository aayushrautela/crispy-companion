// Package autostart registers the companion for login autostart.
//
// Two mechanisms exist, picked by what the platform offers:
//
//   - Linux desktops have the XDG Desktop Background portal: the daemon
//     asks the user once (a real dialog, revocable in Settings) and the
//     desktop writes the autostart entry — the way Pika Backup does it.
//     The portal request itself lives in internal/portal; this package
//     only records the outcome (the State below).
//   - Windows and macOS have no portal. There the daemon writes the
//     platform's own autostart registration itself: a LaunchAgent plist
//     under ~/Library/LaunchAgents on macOS, the HKCU Run value on
//     Windows, and the XDG autostart entry on a Linux host without a
//     portal.
//
// Every entry it writes launches the companion with --autostart-run, so
// the login start serves without registering itself again.
//
// The portal shows a dialog per request, so the outcome is recorded in
// the data dir and settled once: enabled or refused, never asked again
// automatically. A failed registration settles nothing and the next
// start retries. --request-autostart forces a new round, which is the
// way back after the user revoked the app in desktop Settings.
package autostart

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"
)

// EntryName is the file/registry/agent name every mechanism uses, so the
// desktop entry, the LaunchAgent and the Run value can never drift apart.
const EntryName = "io.crispy.Companion"

// Methods recorded in State.Method.
const (
	MethodPortal      = "portal"      // Linux: XDG Desktop Background portal
	MethodXDG         = "xdg"         // Linux without a portal: ~/.config/autostart entry
	MethodLaunchAgent = "launchagent" // macOS: ~/Library/LaunchAgents plist
	MethodRegistry    = "registry"    // Windows: HKCU …\Run value
)

// State records the outcome of the last autostart attempt.
type State struct {
	Enabled     bool      `json:"enabled"`
	Refused     bool      `json:"refused"`
	Method      string    `json:"method,omitempty"`
	RequestedAt time.Time `json:"requestedAt"`
}

// Settled reports whether the state carries a final outcome. Enabled and
// refused both mean "do not ask again"; a failure leaves no state.
func (s State) Settled() bool { return s.Enabled || s.Refused }

// StatePath is where the daemon keeps the outcome, next to the other
// state files in the data dir.
func StatePath(dataDir string) string {
	return filepath.Join(dataDir, "autostart.json")
}

// ReadState loads the recorded outcome. ok is false when the file is
// absent or unreadable — both mean "attempt again".
func ReadState(path string) (State, bool) {
	var st State
	b, err := os.ReadFile(path)
	if err != nil {
		return State{}, false
	}
	if json.Unmarshal(b, &st) != nil {
		return State{}, false
	}
	return st, true
}

// WriteState persists the outcome (0600, like the other stores).
func WriteState(path string, st State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// Register enables login autostart through the platform's own mechanism
// and reports which one was used. exePath is the companion binary to
// launch at login (absolute; callers pass os.Executable()).
//
// macOS: writes the LaunchAgent plist and asks launchd to load it. A
// launchctl failure is logged, not returned: the plist is on disk, so
// the login start works even when launchd cannot be talked to (or the
// agent was already loaded).
func Register(exePath string, logger *log.Logger) (method string, err error) {
	return register(exePath, logger)
}
