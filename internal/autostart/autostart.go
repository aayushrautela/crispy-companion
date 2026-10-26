// Package autostart registers the companion for login autostart.
//
// Two mechanisms exist, picked by what the platform offers:
//
//   - Linux desktops have the XDG Desktop Background portal: the daemon
//     asks and the desktop decides (revocable in its app settings) and
//     writes the autostart entry on the host — the way Pika Backup does
//     it. Inside a Flatpak sandbox that portal is the only mechanism
//     that works: the sandbox sees its own private home, so a direct
//     write lands where the desktop never reads (see InFlatpak).
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
// The outcome is recorded in the data dir. A grant settles the question
// — the entry exists until the user revokes it. A refusal is not final:
// regular launches re-ask, and desktops answer from their permission
// store without another dialog (GNOME). A failed attempt settles
// nothing and the next start retries. --request-autostart forces a new
// round, which is the way back after the user revoked the app in
// desktop Settings.
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

// FlatpakInfoPath is the marker file flatpak writes into the sandbox
// root; its presence is the authoritative sandbox check.
const FlatpakInfoPath = "/.flatpak-info"

// InFlatpak reports whether this process runs inside a Flatpak sandbox.
// Inside one, only the desktop Background portal can arrange login
// autostart: the desktop reads entries from the HOST home, the sandbox
// sees its own private one, and anything a direct registration writes
// here is invisible to it. Callers must therefore answer through the
// portal and report its absence instead of recording a success.
func InFlatpak() bool {
	if _, err := os.Stat(FlatpakInfoPath); err == nil {
		return true
	}
	return os.Getenv("FLATPAK_ID") != ""
}

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
