// Package server wires the companion HTTP surface (PLAN.md §5).
//
// M1 scope: GET /companion/ping, GET /healthz, GET /companion/status,
// POST /companion/revoke, POST /companion/pair (window bookkeeping only;
// the crispy-server device-grant calls land in M2), plus 501/503 stubs for
// the M3 streaming contract so the route table is final early.
package server

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"

	"github.com/crispy-streaming/crispy-companion/internal/engine"
	"github.com/crispy-streaming/crispy-companion/internal/httpx"
	"github.com/crispy-streaming/crispy-companion/internal/notify"
	"github.com/crispy-streaming/crispy-companion/internal/pairing"
)

// Version is set via -ldflags at release time.
var Version = "0.1.0-dev"

// Deps are the server's collaborators.
type Deps struct {
	Policy httpx.Policy
	Tokens *pairing.Store
	Window *pairing.Window
	Engine *engine.Supervisor
	Notify notify.Notifier
	Mode   string // loopback | tsnet | raw (effective shared mode)
}

// New builds the mux with perimeter middleware applied.
func New(d Deps) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/companion/ping", d.handlePing)
	mux.HandleFunc("/healthz", d.handleHealthz)
	mux.HandleFunc("/companion/status", d.handleStatus)
	mux.HandleFunc("/companion/revoke", d.handleRevoke)
	mux.HandleFunc("/companion/pair", d.handlePair)
	// M3 streaming contract stubs (route table final, behavior later).
	mux.HandleFunc("/", d.handleStreamCompat)
	return d.Policy.Middleware(mux)
}

type pingResponse struct {
	Name          string `json:"name"`
	Version       string `json:"version"`
	Protocol      int    `json:"protocol"`
	RequiresLogin bool   `json:"requiresLogin"`
	Paired        bool   `json:"paired"`
}

func (d Deps) handlePing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, pingResponse{
		Name:          "crispy-companion",
		Version:       Version,
		Protocol:      1,
		RequiresLogin: true,
		Paired:        d.Tokens.Count() > 0,
	})
}

func (d Deps) handleHealthz(w http.ResponseWriter, r *http.Request) {
	// Loopback liveness only: require a loopback remote addr.
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host != "127.0.0.1" && host != "::1" && host != "localhost" {
		http.Error(w, "loopback only", http.StatusForbidden)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

type statusResponse struct {
	Paired       bool   `json:"paired"`
	Mode         string `json:"mode"`
	EngineOk     bool   `json:"engineOk"`
	PairingState string `json:"pairingState"` // idle | pending
	PendingFor   string `json:"pendingFor,omitempty"`
}

func (d Deps) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tok := bearerOf(r)
	paired := false
	if tok != "" {
		if _, ok := d.Tokens.Lookup(tok); ok {
			paired = true
		}
	}
	pending, _, _ := d.Window.State()
	state := "idle"
	if pending {
		state = "pending"
	}
	writeJSON(w, http.StatusOK, statusResponse{
		Paired:       paired,
		Mode:         d.Mode,
		EngineOk:     d.Engine.Ready(),
		PairingState: state,
	})
}

func (d Deps) handleRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	requireClientHeader(w, r)
	tok := bearerOf(r)
	if tok == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := d.Tokens.Revoke(tok); err != nil {
		http.Error(w, "revoke failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// handlePair opens the single-pending pairing window (M1: bookkeeping only).
// M2 performs the crispy-server RFC 8628 authorize call and returns the
// verification URL; until then the response is 501 with the pending ID so
// clients can distinguish "window held" from "not implemented".
func (d Deps) handlePair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !requireClientHeader(w, r) {
		return
	}
	id, err := d.Window.Begin()
	switch err {
	case nil:
	case pairing.ErrPendingExists:
		http.Error(w, "pairing already pending", http.StatusConflict)
		return
	case pairing.ErrRateLimited:
		http.Error(w, "too many pairing attempts", http.StatusTooManyRequests)
		return
	default:
		http.Error(w, "pairing unavailable", http.StatusInternalServerError)
		return
	}
	// M2: POST {crispyServer}/v1/auth/device/authorize here, then return
	// {status:"pending", verificationUrl, pendingPairId}. Until the grant
	// driver exists, release the window and report unimplemented so no
	// client mistakes this for a live pairing.
	d.Window.Clear(id)
	http.Error(w, "pairing not yet implemented (M2)", http.StatusNotImplemented)
}

// handleStreamCompat is the M3 route table: infohash-scoped paths.
// M1 behavior: 401 without a valid token, 503 with one (engine mapping
// not yet implemented). This ordering lets auth tests pass early.
func (d Deps) handleStreamCompat(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/")
	seg, _, _ := strings.Cut(rest, "/")
	if !isInfoHash(seg) {
		http.NotFound(w, r)
		return
	}
	tok := bearerOf(r)
	if tok == "" {
		tok = r.URL.Query().Get("token")
	}
	if tok == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if _, ok := d.Tokens.Lookup(tok); !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	http.Error(w, "streaming not yet implemented (M3)", http.StatusServiceUnavailable)
}

// bearerOf extracts a Bearer token. Callers must never log the result.
func bearerOf(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	scheme, token, _ := strings.Cut(h, " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

// requireClientHeader enforces the preflight-forcing X-Crispy-Client header
// on state-changing routes (PLAN §6: simple-form POSTs can't reach handlers).
func requireClientHeader(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-Crispy-Client") == "" {
		http.Error(w, "missing X-Crispy-Client", http.StatusBadRequest)
		return false
	}
	return true
}

func isInfoHash(s string) bool {
	if len(s) == 40 {
		for _, c := range s {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return false
			}
		}
		return true
	}
	if len(s) == 32 {
		for _, c := range s {
			if !((c >= 'A' && c <= 'Z') || (c >= '2' && c <= '7') || (c >= 'a' && c <= 'z')) {
				return false
			}
		}
		return true
	}
	return false
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
