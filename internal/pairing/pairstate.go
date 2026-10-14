package pairing

import (
	"encoding/json"
	"os"
	"time"
)

// PairState is the ephemeral, user-visible view of the current pending
// pairing, mirrored to pairing-state.json so headless/portal-less setups
// (`crispy-companion pair show`) can display the code and verification URL
// without any new HTTP surface. It contains NO secrets: the user code is
// typed by the user themselves and the device code stays memory-only.
type PairState struct {
	PendingPairID   string    `json:"pendingPairId"`
	UserCode        string    `json:"userCode"`
	VerificationURL string    `json:"verificationUrl"`
	ExpiresAt       time.Time `json:"expiresAt"`
}

// WritePairState persists the pending-pairing view (0600, atomic).
func WritePairState(path string, s PairState) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadPairState loads the pending-pairing view. A missing file means no
// pairing is (or ever was) pending; callers must still check ExpiresAt.
func ReadPairState(path string) (PairState, error) {
	var s PairState
	data, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, err
	}
	return s, nil
}

// RemovePairState deletes the view (best-effort; terminals must not fail
// over a convenience file).
func RemovePairState(path string) {
	_ = os.Remove(path)
}
