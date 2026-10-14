// Package pairing owns local companion tokens and the single-pending
// pairing window (PLAN.md §5.2, §7).
//
// M1 scope: persistent 0600 token store + constant-time bearer auth +
// single-pending window bookkeeping + rate limiting. The crispy-server
// RFC 8628 device-grant calls land in M2; this package exposes the seam
// (GrantDriver) so M2 plugs in without changing middleware or handlers.
package pairing

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Token is a minted local companion API token (opaque to clients).
type Token struct {
	UserID     string    `json:"userId"`
	Email      string    `json:"email"`
	DeviceID   string    `json:"deviceId,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	LastSeenAt time.Time `json:"lastSeenAt"`
}

// Store is a 0600 JSON token file: token -> Token.
type Store struct {
	mu   sync.Mutex
	path string
	toks map[string]Token
}

// Open loads (or creates) the token store, enforcing 0600 permissions.
func Open(path string) (*Store, error) {
	s := &Store{path: path, toks: map[string]Token{}}
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
	if err := json.Unmarshal(data, &s.toks); err != nil {
		return nil, fmt.Errorf("parse token store: %w", err)
	}
	return s, nil
}

func (s *Store) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.toks, "", "  ")
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
	return os.Rename(tmp, s.path)
}

// Mint creates a ≥128-bit URL-safe token bound to the given identity.
func (s *Store) Mint(userID, email, deviceID string) (string, error) {
	var raw [24]byte // 192 bits
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(raw[:])
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.toks[tok] = Token{UserID: userID, Email: email, DeviceID: deviceID, CreatedAt: now, LastSeenAt: now}
	if err := s.saveLocked(); err != nil {
		delete(s.toks, tok)
		return "", err
	}
	return tok, nil
}

// Lookup authenticates a bearer token in constant time over the key set.
// Callers must never log the token value.
func (s *Store) Lookup(bearer string) (Token, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.toks {
		if len(k) == len(bearer) && subtle.ConstantTimeCompare([]byte(k), []byte(bearer)) == 1 {
			v.LastSeenAt = time.Now().UTC()
			s.toks[k] = v
			return v, true
		}
	}
	return Token{}, false
}

// Revoke deletes a token. Unknown tokens are a no-op success.
func (s *Store) Revoke(bearer string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.toks {
		if len(k) == len(bearer) && subtle.ConstantTimeCompare([]byte(k), []byte(bearer)) == 1 {
			delete(s.toks, k)
			return s.saveLocked()
		}
	}
	return nil
}

// RevokeAll wipes every local token (server-side revocation detected).
func (s *Store) RevokeAll() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.toks = map[string]Token{}
	return s.saveLocked()
}

// Count returns the number of stored tokens (for /status paired flag).
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.toks)
}

// Window is the single-pending pairing state machine (PLAN §5.2).
type Window struct {
	mu        sync.Mutex
	pendingID string
	expiresAt time.Time
	attempts  []time.Time // handshake attempts for rate limiting
	maxPerMin int
	ttl       time.Duration
}

// NewWindow creates a pairing window with the configured TTL + rate limit.
func NewWindow(ttl time.Duration, maxPerMin int) *Window {
	return &Window{ttl: ttl, maxPerMin: maxPerMin}
}

var (
	ErrPendingExists = errors.New("pairing already pending")
	ErrRateLimited   = errors.New("too many pairing attempts")
)

// Begin starts a pairing window, returning its pending ID.
// Exactly one pending pairing may exist; attempts are rate-limited.
func (w *Window) Begin() (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	if w.pendingID != "" && now.Before(w.expiresAt) {
		return "", ErrPendingExists
	}
	// Prune attempts older than a minute.
	kept := w.attempts[:0]
	for _, t := range w.attempts {
		if now.Sub(t) < time.Minute {
			kept = append(kept, t)
		}
	}
	w.attempts = kept
	if len(w.attempts) >= w.maxPerMin {
		return "", ErrRateLimited
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	w.pendingID = base64.RawURLEncoding.EncodeToString(raw[:])
	w.expiresAt = now.Add(w.ttl)
	w.attempts = append(w.attempts, now)
	return w.pendingID, nil
}

// State reports the current window state for /status.
func (w *Window) State() (pending bool, pendingFor string, expiresAt time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pendingID == "" || time.Now().After(w.expiresAt) {
		return false, "", time.Time{}
	}
	return true, w.pendingID, w.expiresAt
}

// Clear ends the pending window (on approve/deny/expiry/consume).
func (w *Window) Clear(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if id == "" || w.pendingID == id {
		w.pendingID = ""
	}
}
