package grant_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crispy-streaming/crispy-companion/internal/grant"
)

func envelope(data any) map[string]any {
	return map[string]any{"data": data, "meta": map[string]string{"requestId": "test"}}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// fakeServer mimics the crispy-server device-grant envelopes.
func fakeServer(t *testing.T, pollStatus string, pollsBeforeApproved int) *httptest.Server {
	t.Helper()
	var polls int
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/device/authorize":
			var body struct {
				ClientID   string `json:"clientId"`
				DeviceName string `json:"deviceName"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.ClientID != "crispy-companion" {
				writeJSON(w, 400, map[string]any{"error": map[string]any{"code": "invalid_client_id", "message": "Unknown client ID."}})
				return
			}
			writeJSON(w, 201, envelope(map[string]any{
				"deviceCode":              "cp_dvc_testcode",
				"userCode":                "ABCD-EFGH",
				"verificationUri":         "https://app.example.test/device",
				"verificationUriComplete": "https://app.example.test/device?user_code=ABCD-EFGH",
				"expiresIn":               900,
				"interval":                5,
			}))
		case "/v1/auth/device/token":
			polls++
			if pollStatus == "approved" && polls > pollsBeforeApproved {
				writeJSON(w, 200, envelope(map[string]any{
					"status":         "approved",
					"plaintextToken": "cp_pat_testtoken",
					"deviceId":       "00000000-0000-0000-0000-000000000001",
					"token":          map[string]any{"id": "t1", "name": "TV session", "tokenPreview": "cp_pat_test", "scopes": []string{}, "expiresAt": nil, "createdAt": "2026-01-01"},
					"user":           map[string]any{"id": "user-1", "email": "alice@example.com"},
				}))
				return
			}
			status := pollStatus
			if pollStatus == "approved" {
				status = "authorization_pending" // pre-threshold: still waiting
			}
			writeJSON(w, 200, envelope(map[string]any{"status": status}))
		case "/v1/auth/devices":
			writeJSON(w, 200, envelope(map[string]any{"devices": []any{}}))
		default:
			w.WriteHeader(404)
		}
	}))
}

func TestAuthorizeOK(t *testing.T) {
	srv := fakeServer(t, "authorization_pending", 0)
	defer srv.Close()
	c := grant.New(srv.URL)
	a, err := c.Authorize(context.Background(), "test device", "")
	if err != nil {
		t.Fatal(err)
	}
	if a.DeviceCode != "cp_dvc_testcode" || a.UserCode != "ABCD-EFGH" {
		t.Fatalf("bad authorize: %+v", a)
	}
	if a.VerificationURIComplete == "" || a.ExpiresIn != 900*time.Second {
		t.Fatalf("bad authorize fields: %+v", a)
	}
}

func TestAuthorizeUnknownClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 400, map[string]any{"error": map[string]any{"code": "invalid_client_id", "message": "Unknown client ID."}})
	}))
	defer srv.Close()
	c := grant.New(srv.URL)
	// Force a wrong client by pointing at a server that always rejects.
	_, err := c.Authorize(context.Background(), "d", "")
	if err != grant.ErrUnknownClient {
		t.Fatalf("got %v, want ErrUnknownClient", err)
	}
}

func TestAuthorizeSurvivesRedirect(t *testing.T) {
	// A deployment behind an nginx http→https redirect answers 301. Go would
	// rewrite the POST to a GET and drop the body, so the API would reject
	// the grant with "must have required property 'clientId'".
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method across redirect = %q, want POST", r.Method)
		}
		var body struct {
			ClientID string `json:"clientId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.ClientID != "crispy-companion" {
			t.Errorf("clientId lost across redirect: %q", body.ClientID)
		}
		writeJSON(w, 201, envelope(map[string]any{
			"deviceCode":              "cp_dvc_testcode",
			"userCode":                "ABCD-EFGH",
			"verificationUri":         "https://app.example.test/device",
			"verificationUriComplete": "https://app.example.test/device?user_code=ABCD-EFGH",
			"expiresIn":               900,
			"interval":                5,
		}))
	}))
	defer origin.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, origin.URL+r.URL.Path, http.StatusMovedPermanently)
	}))
	defer redirector.Close()

	c := grant.New(redirector.URL)
	a, err := c.Authorize(context.Background(), "test device", "")
	if err != nil {
		t.Fatal(err)
	}
	if a.DeviceCode != "cp_dvc_testcode" || a.UserCode != "ABCD-EFGH" {
		t.Fatalf("bad authorize: %+v", a)
	}
}

func TestPollPendingAndApproved(t *testing.T) {
	srv := fakeServer(t, "approved", 1)
	defer srv.Close()
	c := grant.New(srv.URL)
	oc, err := c.Poll(context.Background(), "cp_dvc_testcode")
	if err != nil {
		t.Fatal(err)
	}
	if oc.Status != "authorization_pending" {
		t.Fatalf("first poll: %q", oc.Status)
	}
	oc, err = c.Poll(context.Background(), "cp_dvc_testcode")
	if err != nil {
		t.Fatal(err)
	}
	if oc.Status != "approved" || oc.Approved == nil {
		t.Fatalf("second poll: %+v", oc)
	}
	if oc.Approved.Email != "alice@example.com" || oc.Approved.UserID != "user-1" {
		t.Fatalf("bad identity: %+v", oc.Approved)
	}
}

func TestPollDeniedExpired(t *testing.T) {
	for _, status := range []string{"access_denied", "expired_token"} {
		srv := fakeServer(t, status, 0)
		c := grant.New(srv.URL)
		oc, err := c.Poll(context.Background(), "cp_dvc_testcode")
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if oc.Status != status {
			t.Fatalf("got %q, want %q", oc.Status, status)
		}
	}
}

func TestRevalidateAliveAndDead(t *testing.T) {
	srv := fakeServer(t, "authorization_pending", 0)
	defer srv.Close()
	c := grant.New(srv.URL)
	if err := c.Revalidate(context.Background(), "cp_pat_live"); err != nil {
		t.Fatalf("live PAT (200): %v", err)
	}

	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
	}))
	defer forbidden.Close()
	c2 := grant.New(forbidden.URL)
	if err := c2.Revalidate(context.Background(), "cp_pat_live"); err != nil {
		t.Fatalf("live PAT (403, not a session): %v", err)
	}

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
	}))
	defer dead.Close()
	c3 := grant.New(dead.URL)
	if err := c3.Revalidate(context.Background(), "cp_pat_dead"); err != grant.ErrPATDead {
		t.Fatalf("dead PAT: got %v, want ErrPATDead", err)
	}
}
