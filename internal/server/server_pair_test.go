package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crispy-streaming/crispy-companion/internal/pairing"
)

func testEnvelope(data any) map[string]any {
	return map[string]any{"data": data, "meta": map[string]string{"requestId": "test"}}
}

// fakeGrantServer mimics crispy-server's device-grant endpoints. After
// approveAfter polls it returns approved; verdict "denied"/"expired"
// returns that terminal state immediately.
func fakeGrantServer(verdict string, approveAfter int64) *httptest.Server {
	var polls int64
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/auth/device/authorize":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(testEnvelope(map[string]any{
				"deviceCode":              "cp_dvc_e2e",
				"userCode":                "WXYZ-BCDF",
				"verificationUri":         "https://app.example.test/device",
				"verificationUriComplete": "https://app.example.test/device?user_code=WXYZ-BCDF",
				"expiresIn":               900,
				"interval":                5,
			}))
		case "/v1/auth/device/token":
			n := atomic.AddInt64(&polls, 1)
			if verdict == "approved" && n > approveAfter {
				_ = json.NewEncoder(w).Encode(testEnvelope(map[string]any{
					"status":         "approved",
					"plaintextToken": "cp_pat_e2e",
					"deviceId":       "00000000-0000-0000-0000-000000000002",
					"token":          map[string]any{"id": "t1", "name": "s", "tokenPreview": "cp_pat_e2e", "scopes": []string{}, "expiresAt": nil, "createdAt": "2026-01-01"},
					"user":           map[string]any{"id": "user-9", "email": "bob@example.com"},
				}))
				return
			}
			if verdict == "approved" {
				_ = json.NewEncoder(w).Encode(testEnvelope(map[string]any{"status": "authorization_pending"}))
				return
			}
			status := "access_denied"
			if verdict == "expired" {
				status = "expired_token"
			}
			_ = json.NewEncoder(w).Encode(testEnvelope(map[string]any{"status": status}))
		default:
			w.WriteHeader(404)
		}
	}))
}

func pairHeaders() map[string]string {
	return map[string]string{"Origin": "https://crispy.app", "X-Crispy-Client": "crispy-web/1"}
}

func TestPairApproveEndToEnd(t *testing.T) {
	srv := fakeGrantServer("approved", 0)
	defer srv.Close()
	h, store, dir := testHandlerWithServer(t, srv.URL)

	rec := doReq(h, http.MethodPost, "http://127.0.0.1:11470/companion/pair", pairHeaders())
	if rec.Code != http.StatusOK {
		t.Fatalf("pair: got %d, body %s", rec.Code, rec.Body.String())
	}
	var started struct {
		Status          string `json:"status"`
		PendingPairID   string `json:"pendingPairId"`
		UserCode        string `json:"userCode"`
		VerificationURL string `json:"verificationUrl"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&started); err != nil {
		t.Fatal(err)
	}
	if started.Status != "pending" || started.PendingPairID == "" || started.UserCode == "" || started.VerificationURL == "" {
		t.Fatalf("bad pair response: %+v", started)
	}

	// Headless mirror written for `pair show` (no secrets inside).
	st, err := pairing.ReadPairState(filepath.Join(dir, "pairing-state.json"))
	if err != nil {
		t.Fatalf("pair-state mirror: %v", err)
	}
	if st.UserCode != started.UserCode || st.PendingPairID != started.PendingPairID || st.VerificationURL == "" {
		t.Fatalf("bad pair-state mirror: %+v", st)
	}

	// Second pair while pending must conflict, never queue.
	rec2 := doReq(h, http.MethodPost, "http://127.0.0.1:11470/companion/pair", pairHeaders())
	if rec2.Code != http.StatusConflict {
		t.Fatalf("second pair: got %d, want 409", rec2.Code)
	}

	// Poll the pending-scoped status until the server approves (first
	// poll fires ~5s in, per the grant interval floor).
	var token string
	deadline := time.Now().Add(30 * time.Second)
	for {
		rec := doReq(h, http.MethodGet,
			"http://127.0.0.1:11470/companion/status?pendingPairId="+started.PendingPairID,
			map[string]string{"Origin": "https://crispy.app"})
		var st struct {
			PairingState string `json:"pairingState"`
			Token        string `json:"token"`
			User         struct {
				ID    string `json:"id"`
				Email string `json:"email"`
			} `json:"user"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&st); err != nil {
			t.Fatal(err)
		}
		if st.PairingState == "approved" {
			token = st.Token
			if st.User.Email != "bob@example.com" {
				t.Fatalf("wrong verified identity: %+v", st.User)
			}
			break
		}
		if st.PairingState != "pending" {
			t.Fatalf("unexpected state %q", st.PairingState)
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for approval")
		}
		time.Sleep(500 * time.Millisecond)
	}
	if token == "" {
		t.Fatal("no local token delivered")
	}

	// Single-read: the token must not be delivered twice.
	rec = doReq(h, http.MethodGet,
		"http://127.0.0.1:11470/companion/status?pendingPairId="+started.PendingPairID,
		map[string]string{"Origin": "https://crispy.app"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("replay read: got %d, want 404", rec.Code)
	}
	// Mirror cleared on terminal delivery.
	if _, err := os.Stat(filepath.Join(dir, "pairing-state.json")); !os.IsNotExist(err) {
		t.Fatal("pair-state mirror not cleared after delivery")
	}

	// The minted token authenticates the torrent surface. No engine client is
	// wired in this harness, so the /v1 status route answers 503 (authed,
	// engine unavailable) rather than 401/403.
	rec = doReq(h, http.MethodGet, "http://127.0.0.1:11470/v1/torrents/"+testHash,
		map[string]string{"Origin": "https://crispy.app", "Authorization": "Bearer " + token})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("authed /v1 status: got %d, want 503", rec.Code)
	}
	if n := store.Count(); n != 1 {
		t.Fatalf("token store count = %d, want 1", n)
	}
}

func TestPairDeniedEndToEnd(t *testing.T) {
	srv := fakeGrantServer("denied", 0)
	defer srv.Close()
	h, _, _ := testHandlerWithServer(t, srv.URL)

	rec := doReq(h, http.MethodPost, "http://127.0.0.1:11470/companion/pair", pairHeaders())
	var started struct {
		PendingPairID string `json:"pendingPairId"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&started); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		rec := doReq(h, http.MethodGet,
			"http://127.0.0.1:11470/companion/status?pendingPairId="+started.PendingPairID,
			map[string]string{"Origin": "https://crispy.app"})
		var st struct {
			PairingState string `json:"pairingState"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&st); err != nil {
			t.Fatal(err)
		}
		if st.PairingState == "denied" {
			return
		}
		if st.PairingState != "pending" {
			t.Fatalf("unexpected state %q", st.PairingState)
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for denial")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func TestPairUnknownClientFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": "invalid_client_id", "message": "Unknown client ID."},
		})
	}))
	defer srv.Close()
	h, _, _ := testHandlerWithServer(t, srv.URL)
	rec := doReq(h, http.MethodPost, "http://127.0.0.1:11470/companion/pair", pairHeaders())
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unknown client: got %d, want 503", rec.Code)
	}
	// Window must be released: a retry may proceed (still 503, not 409).
	rec = doReq(h, http.MethodPost, "http://127.0.0.1:11470/companion/pair", pairHeaders())
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("retry after unknown client: got %d, want 503", rec.Code)
	}
}
