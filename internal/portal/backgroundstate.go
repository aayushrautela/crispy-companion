package portal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// BackgroundState records how the last RequestBackground round went, so
// the daemon asks the desktop at most once. The Background portal shows
// the user a dialog on every request — asking on each start would nag the
// very people who declined.
//
// Policy (Pika Backup-style):
//   - granted or refused → settled, never ask again automatically
//   - request failed, never asked, or no state file → ask on next start
//   - --request-autostart forces a new round either way (e.g. after the
//     user revoked the app in desktop Settings, which removes the
//     autostart entry but not this file)
type BackgroundState struct {
	Granted     bool      `json:"granted"`
	Refused     bool      `json:"refused"`
	RequestedAt time.Time `json:"requestedAt"`
}

// Settled reports whether the state carries a final user answer.
func (s BackgroundState) Settled() bool { return s.Granted || s.Refused }

// BackgroundStatePath is where the daemon keeps the request outcome,
// alongside the other state files in the data dir.
func BackgroundStatePath(dataDir string) string {
	return filepath.Join(dataDir, "background-portal.json")
}

// ReadBackgroundState loads the recorded outcome. ok is false when the
// file is absent or unreadable — both mean "ask again".
func ReadBackgroundState(path string) (BackgroundState, bool) {
	var st BackgroundState
	b, err := os.ReadFile(path)
	if err != nil {
		return BackgroundState{}, false
	}
	if json.Unmarshal(b, &st) != nil {
		return BackgroundState{}, false
	}
	return st, true
}

// WriteBackgroundState persists the outcome (0600, like the other stores).
func WriteBackgroundState(path string, st BackgroundState) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
