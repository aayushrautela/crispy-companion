package httpx_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crispy-streaming/crispy-companion/internal/httpx"
)

func testPolicy() httpx.Policy {
	return httpx.NewPolicy(
		[]string{"127.0.0.1:11470", "localhost:11470"},
		[]string{"https://crispy.app"},
		false,
	)
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func TestHostRebindingRejected(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:11470/companion/ping", nil)
	req.Host = "evil.com:11470"
	req.Header.Set("Origin", "https://crispy.app")
	rec := httptest.NewRecorder()
	testPolicy().Middleware(okHandler()).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("rebind host: got %d, want 403", rec.Code)
	}
}

func TestCrossSiteRejected(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:11470/companion/ping", nil)
	req.Header.Set("Origin", "https://crispy.app")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	testPolicy().Middleware(okHandler()).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site: got %d, want 403", rec.Code)
	}
}

func TestDisallowedOriginRejected(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:11470/companion/ping", nil)
	req.Header.Set("Origin", "https://evil.com")
	rec := httptest.NewRecorder()
	testPolicy().Middleware(okHandler()).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("evil origin: got %d, want 403", rec.Code)
	}
}

func TestAllowedOriginReflectedNeverStar(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:11470/companion/ping", nil)
	req.Header.Set("Origin", "https://crispy.app")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	testPolicy().Middleware(okHandler()).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("allowed origin: got %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://crispy.app" {
		t.Fatalf("ACAO = %q, want exact origin reflection", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Private-Network"); got != "true" {
		t.Fatalf("PNA header = %q, want true", got)
	}
}

func TestPreflight(t *testing.T) {
	req := httptest.NewRequest(http.MethodOptions, "http://127.0.0.1:11470/companion/pair", nil)
	req.Header.Set("Origin", "https://crispy.app")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	testPolicy().Middleware(okHandler()).ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight: got %d, want 204", rec.Code)
	}
}

func TestAbsentOriginPassesMiddleware(t *testing.T) {
	// <video> no-cors requests carry no Origin; middleware lets them
	// through, per-endpoint auth decides.
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:11470/companion/ping", nil)
	rec := httptest.NewRecorder()
	testPolicy().Middleware(okHandler()).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("absent origin: got %d, want 200", rec.Code)
	}
}
