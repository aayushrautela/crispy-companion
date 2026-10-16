package server_test

import (
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/crispy-streaming/crispy-companion/internal/engine"
	"github.com/crispy-streaming/crispy-companion/internal/grant"
	"github.com/crispy-streaming/crispy-companion/internal/httpx"
	"github.com/crispy-streaming/crispy-companion/internal/notify"
	"github.com/crispy-streaming/crispy-companion/internal/pairing"
	"github.com/crispy-streaming/crispy-companion/internal/server"
)

func testHandler(t *testing.T) (http.Handler, *pairing.Store) {
	t.Helper()
	h, store, _ := testHandlerWithServer(t, "")
	return h, store
}

func testHandlerWithServer(t *testing.T, serverURL string) (http.Handler, *pairing.Store, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := pairing.Open(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := pairing.OpenSession(filepath.Join(dir, "device-session.json"))
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(engine.Config{Addr: "127.0.0.1", Port: 18090})
	if err != nil {
		t.Fatal(err)
	}
	h := server.New(server.Deps{
		Policy: httpx.NewPolicy(
			[]string{"127.0.0.1:11470", "localhost:11470"},
			[]string{"https://crispy.app"},
			false,
		),
		Tokens:           store,
		Window:           pairing.NewWindow(90*time.Second, 5),
		Engine:           eng,
		Notify:           notify.LogNotifier{Log: log.New(os.Stderr, "test: ", 0)},
		Mode:             "loopback",
		Grant:            grant.New(serverURL),
		Session:          session,
		DeviceName:       "test device",
		PairingStatePath: filepath.Join(dir, "pairing-state.json"),
	})
	return h, store, dir
}

func doReq(h http.Handler, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPingPublic(t *testing.T) {
	h, _ := testHandler(t)
	rec := doReq(h, http.MethodGet, "http://127.0.0.1:11470/companion/ping",
		map[string]string{"Origin": "https://crispy.app"})
	if rec.Code != http.StatusOK {
		t.Fatalf("ping: got %d", rec.Code)
	}
}

// The inherited root-level stream routes are gone (PLAN D13); the native
// /v1 surface is the only torrent surface now.
func TestLegacyStreamRoutesAreGone(t *testing.T) {
	h, store := testHandler(t)
	tok, err := store.Mint("u1", "alice@example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	ih := "da39a3ee5e6b4b0d3255bfef95601890afd80709"
	for _, target := range []string{"/" + ih + "/create", "/" + ih + "/0/stats.json", "/" + ih + "/0"} {
		rec := doReq(h, http.MethodGet, "http://127.0.0.1:11470"+target,
			map[string]string{
				"Origin":        "https://crispy.app",
				"Authorization": "Bearer " + tok,
			})
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: got %d, want 404", target, rec.Code)
		}
	}
}

func TestRevokeNeedsClientHeaderAndToken(t *testing.T) {
	h, _ := testHandler(t)
	rec := doReq(h, http.MethodPost, "http://127.0.0.1:11470/companion/revoke",
		map[string]string{"Origin": "https://crispy.app"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("revoke w/o client header: got %d, want 400", rec.Code)
	}
	rec = doReq(h, http.MethodPost, "http://127.0.0.1:11470/companion/revoke",
		map[string]string{"Origin": "https://crispy.app", "X-Crispy-Client": "crispy-web/1"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoke w/o token: got %d, want 401", rec.Code)
	}
}

func TestPairRequiresClientHeader(t *testing.T) {
	h, _ := testHandler(t)
	rec := doReq(h, http.MethodPost, "http://127.0.0.1:11470/companion/pair",
		map[string]string{"Origin": "https://crispy.app"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("pair w/o client header: got %d, want 400", rec.Code)
	}
}

func TestPairFailClosedWithoutServer(t *testing.T) {
	h, _ := testHandler(t) // empty server URL
	rec := doReq(h, http.MethodPost, "http://127.0.0.1:11470/companion/pair",
		map[string]string{"Origin": "https://crispy.app", "X-Crispy-Client": "crispy-web/1"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("pair w/o server: got %d, want 503", rec.Code)
	}
}
