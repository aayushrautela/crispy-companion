// Package server wires the companion HTTP surface (PLAN.md §5).
//
// M1 scope: GET /companion/ping, GET /healthz, GET /companion/status,
// POST /companion/revoke, POST /companion/pair (window bookkeeping only;
// the crispy-server device-grant calls land in M2), plus 501/503 stubs for
// the M3 streaming contract so the route table is final early.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/crispy-streaming/crispy-companion/internal/engine"
	"github.com/crispy-streaming/crispy-companion/internal/grant"
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
	// Grant talks to crispy-server. A zero ServerURL means pairing is
	// unconfigured: /pair fails closed with 503 (PLAN D8).
	Grant *grant.Client
	// Session persists the server-minted cp_pat_ + device row.
	Session *pairing.SessionFile
	// DeviceName is the human label sent to the server, e.g.
	// "Crispy Companion on hostname".
	DeviceName string
	// Torrents speaks the engine API; Links remembers add links for
	// transparent re-adds after engine idle drops. MediaSem caps
	// concurrent proxied streams (nil → default in New).
	Torrents *engine.Client
	Links    *engine.LinkStore
	MediaSem chan struct{}
	// PairingStatePath mirrors the pending pairing's user-visible facts
	// (pairing-state.json) for headless `pair show`. Empty disables.
	PairingStatePath string
}

// New builds the mux with perimeter middleware applied.
func New(d Deps) http.Handler {
	if d.MediaSem == nil {
		d.MediaSem = make(chan struct{}, maxMediaProxies)
	}
	if d.Links == nil {
		d.Links = engine.NewLinkStore()
	}
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
	// Pending-scoped status for the pairing browser, which holds no token
	// yet. The pendingPairId authenticates this read; the approved local
	// token is delivered exactly once via Window.Take.
	if pid := r.URL.Query().Get("pendingPairId"); pid != "" {
		d.handlePendingStatus(w, pid)
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

// handlePendingStatus serves GET /companion/status?pendingPairId=….
func (d Deps) handlePendingStatus(w http.ResponseWriter, pid string) {
	pending, current, _ := d.Window.State()
	if !pending || subtle.ConstantTimeCompare([]byte(pid), []byte(current)) != 1 {
		http.Error(w, "unknown pairing", http.StatusNotFound)
		return
	}
	if o, ok := d.take(pid); ok {
		switch o.Status {
		case pairing.OutcomeApproved:
			writeJSON(w, http.StatusOK, map[string]any{
				"pairingState":  "approved",
				"pendingPairId": pid,
				"token":         o.LocalToken,
				"user":          map[string]string{"id": o.UserID, "email": o.Email},
			})
		case pairing.OutcomeDenied:
			writeJSON(w, http.StatusOK, map[string]string{
				"pairingState": "denied", "pendingPairId": pid,
			})
		default:
			writeJSON(w, http.StatusOK, map[string]string{
				"pairingState": "expired", "pendingPairId": pid,
			})
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"pairingState": "pending", "pendingPairId": pid,
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

// handlePair starts RFC 8628 pairing via crispy-server (PLAN §5.2).
//
// The companion calls POST {server}/v1/auth/device/authorize directly and
// returns the verification URL. Approval happens in the logged-in webapp;
// the companion polls the server and mints a local token on approval.
// New pairings fail closed when the server is unreachable/unconfigured.
func (d Deps) handlePair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !requireClientHeader(w, r) {
		return
	}
	if d.Grant == nil || d.Grant.ServerURL == "" {
		http.Error(w, "pairing unavailable: server not configured", http.StatusServiceUnavailable)
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
	fail := func(code int, msg string) {
		d.Window.Clear(id)
		http.Error(w, msg, code)
	}

	deviceName := d.DeviceName
	if name := pairDeviceName(r); name != "" {
		deviceName = name
	}
	var claimed string
	if ds, ok := d.Session.Get(); ok {
		claimed = ds.DeviceID // server reuses the row when it still belongs to us
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	auth, err := d.Grant.Authorize(ctx, deviceName, claimed)
	if err != nil {
		switch {
		case err == grant.ErrUnknownClient:
			fail(http.StatusServiceUnavailable, "pairing unavailable: server update required")
		case err == grant.ErrRateLimited:
			fail(http.StatusTooManyRequests, "too many pairing attempts")
		case ctx.Err() != nil:
			fail(http.StatusServiceUnavailable, "pairing unavailable: cannot reach server")
		default:
			fail(http.StatusBadGateway, "pairing unavailable: cannot reach server")
		}
		return
	}
	deadline := time.Now().Add(auth.ExpiresIn)
	if exp, ok := d.Window.ExpiresAt(id); ok && exp.Before(deadline) {
		deadline = exp
	}
	if !d.Window.Attach(id, pairing.Grant{
		DeviceCode:      auth.DeviceCode,
		UserCode:        auth.UserCode,
		VerificationURL: auth.VerificationURIComplete,
		ServerDeadline:  deadline,
		Interval:        auth.Interval,
	}) {
		fail(http.StatusConflict, "pairing already pending")
		return
	}
	// Mirror the user-visible pairing facts for headless setups
	// (`crispy-companion pair show`). Best-effort: no secrets inside.
	if d.PairingStatePath != "" {
		_ = pairing.WritePairState(d.PairingStatePath, pairing.PairState{
			PendingPairID:   id,
			UserCode:        auth.UserCode,
			VerificationURL: auth.VerificationURIComplete,
			ExpiresAt:       deadline,
		})
	}
	go d.pollPairing(id, auth.Interval, deadline)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "pending",
		"pendingPairId":   id,
		"userCode":        auth.UserCode,
		"verificationUrl": auth.VerificationURIComplete,
		"expiresIn":       int(time.Until(deadline).Seconds()),
		"interval":        int(auth.Interval.Seconds()),
	})
}

// complete records the terminal pairing outcome and clears the headless
// pairing-state mirror (best-effort: terminals must not fail over a
// convenience file).
func (d Deps) complete(id string, o pairing.Outcome) bool {
	ok := d.Window.Complete(id, o)
	if d.PairingStatePath != "" {
		pairing.RemovePairState(d.PairingStatePath)
	}
	return ok
}

// take reads (once) the terminal outcome and clears the headless mirror.
func (d Deps) take(pid string) (pairing.Outcome, bool) {
	o, ok := d.Window.Take(pid)
	if ok && d.PairingStatePath != "" {
		pairing.RemovePairState(d.PairingStatePath)
	}
	return o, ok
}

// pollPairing polls the server until approval/denial/expiry or the window
// ends. Exactly one terminal Complete is recorded; restarts never resurrect
// a window (device codes stay memory-only).
func (d Deps) pollPairing(id string, interval time.Duration, deadline time.Time) {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		<-timer.C
		// Window gone or superseded: stop silently.
		g, ok := d.Window.GrantOf(id)
		if !ok {
			return
		}
		if time.Now().After(deadline) {
			d.complete(id, pairing.Outcome{Status: pairing.OutcomeExpired})
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		oc, err := d.Grant.Poll(ctx, g.DeviceCode)
		cancel()
		if err != nil {
			// Transient: keep polling until the deadline (fail-closed only
			// for *new* pairings; an in-flight attempt rides out blips).
			timer.Reset(g.Interval)
			continue
		}
		switch oc.Status {
		case "authorization_pending":
			timer.Reset(g.Interval)
		case "slow_down":
			if oc.Interval > 0 {
				g.Interval = oc.Interval
			} else {
				g.Interval += 5 * time.Second
			}
			timer.Reset(g.Interval)
		case "access_denied":
			d.complete(id, pairing.Outcome{Status: pairing.OutcomeDenied})
			return
		case "expired_token":
			d.complete(id, pairing.Outcome{Status: pairing.OutcomeExpired})
			return
		case "approved":
			d.finishApproval(id, oc.Approved)
			return
		default:
			timer.Reset(g.Interval)
		}
	}
}

// finishApproval stores the server session, mints the local token bound to
// the approved identity, and posts the confirmation notification.
func (d Deps) finishApproval(id string, ap *grant.Approved) {
	if ap == nil {
		d.complete(id, pairing.Outcome{Status: pairing.OutcomeExpired})
		return
	}
	if err := d.Session.Save(pairing.DeviceSession{
		PlaintextPAT: ap.PlaintextToken,
		DeviceID:     ap.DeviceID,
		UserID:       ap.UserID,
		Email:        ap.Email,
	}); err != nil {
		d.complete(id, pairing.Outcome{Status: pairing.OutcomeExpired})
		return
	}
	local, err := d.Tokens.Mint(ap.UserID, ap.Email, ap.DeviceID)
	if err != nil {
		d.complete(id, pairing.Outcome{Status: pairing.OutcomeExpired})
		return
	}
	_ = d.Tokens.SweepDevice(ap.DeviceID, local)
	d.complete(id, pairing.Outcome{
		Status:     pairing.OutcomeApproved,
		LocalToken: local,
		UserID:     ap.UserID,
		Email:      ap.Email,
	})
	_ = d.Notify.Post(notify.Notification{
		Title: "Crispy Companion paired",
		Body:  "Paired for " + displayEmail(ap.Email),
		Actions: []notify.Action{
			{ID: "revoke", Label: "Revoke"},
		},
	})
}

// pairDeviceName reads an optional {device_name} JSON body (4 KiB cap).
// Empty/missing body keeps the hostname-derived default.
func pairDeviceName(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	var v struct {
		DeviceName string `json:"device_name"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4097))
	if err != nil || len(body) == 0 {
		return ""
	}
	if json.Unmarshal(body, &v) != nil {
		return ""
	}
	name := strings.TrimSpace(v.DeviceName)
	if len(name) > 80 {
		name = name[:80]
	}
	return name
}

// displayEmail renders the verified identity for the confirmation
// notification. Lookalike addresses are the residual risk (PLAN T7);
// showing the full address is the defense.
func displayEmail(email string) string {
	if email == "" {
		return "(unknown account)"
	}
	return email
}

// handleStreamCompat lives in stream.go (M3): Stremio-compatible create,
// stats, and proxied media over the engine client.

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
