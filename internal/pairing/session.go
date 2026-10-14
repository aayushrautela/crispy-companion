package pairing

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// DeviceSession is the server-side session for this companion: the
// device-scoped cp_pat_ minted by crispy-server after user approval,
// plus the server's device row ID. Stored 0600, never logged.
type DeviceSession struct {
	// PlaintextPAT is the cp_pat_ token. Never log, never transmit except
	// as a Bearer to crispy-server over TLS.
	PlaintextPAT string `json:"plaintextPat"`
	DeviceID     string `json:"deviceId"`
	UserID       string `json:"userId"`
	Email        string `json:"email"`
}

// SessionFile persists one DeviceSession (0600).
type SessionFile struct {
	path    string
	current *DeviceSession // unexported: no String method, never log
}

// OpenSession loads (or creates empty) the device session file.
func OpenSession(path string) (*SessionFile, error) {
	s := &SessionFile{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return s, nil
	}
	var ds DeviceSession
	if err := json.Unmarshal(data, &ds); err != nil {
		return nil, err
	}
	s.current = &ds
	return s, nil
}

// Get returns the stored session, if any.
func (s *SessionFile) Get() (DeviceSession, bool) {
	if s.current == nil {
		return DeviceSession{}, false
	}
	return *s.current, true
}

// Save persists the session with 0600 permissions (atomic rename).
func (s *SessionFile) Save(ds DeviceSession) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(ds)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.current = &ds
	return nil
}

// Clear removes the session file and memory.
func (s *SessionFile) Clear() error {
	s.current = nil
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
