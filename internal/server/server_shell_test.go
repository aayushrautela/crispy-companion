package server_test

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
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

// shellTestHandler builds a companion with the shell-token endpoint enabled
// (allow) or not. The engine client points at a dead port: /v1 auth runs
// before any engine call, so 401-vs-not is deterministic.
func shellTestHandler(t *testing.T, allow bool) (http.Handler, *pairing.Store) {
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
	eng, err := engine.New(engine.Config{Addr: "127.0.0.1", Port: 18097})
	if err != nil {
		t.Fatal(err)
	}
	h := server.New(server.Deps{
		Policy: httpx.NewPolicy(
			[]string{"127.0.0.1:11470", "localhost:11470"},
			[]string{"https://crispy.app"}, false),
		Tokens:           store,
		Window:           pairing.NewWindow(90*time.Second, 5),
		Engine:           eng,
		Notify:           notify.LogNotifier{Log: log.New(io.Discard, "", 0)},
		Mode:             "loopback",
		Grant:            grant.New(""),
		Session:          session,
		DeviceName:       "test device",
		Torrents:         engine.NewClient("http://127.0.0.1:1"),
		Links:            engine.NewLinkStore(),
		AllowShellToken:  allow,
	})
	return h, store
}

func postShellToken(t *testing.T, h http.Handler, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:11470/companion/shell-token", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestShellTokenDisabledByDefault(t *testing.T) {
	h, store := shellTestHandler(t, false)
	rec := postShellToken(t, h, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("disabled: got %d, want 404", rec.Code)
	}
	if store.Count() != 0 {
		t.Fatal("disabled endpoint minted a token")
	}
}

func TestShellTokenNonBrowserCaller(t *testing.T) {
	h, store := shellTestHandler(t, true)

	rec := postShellToken(t, h, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Token    string `json:"token"`
		DeviceID string `json:"deviceId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Token == "" || body.DeviceID != pairing.ShellDeviceID {
		t.Fatalf("bad body: %+v", body)
	}
	if _, ok := store.Lookup(body.Token); !ok {
		t.Fatal("minted token not in store")
	}

	// Stable across calls: the same shell token is returned again.
	rec2 := postShellToken(t, h, nil)
	var body2 struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &body2); err != nil {
		t.Fatal(err)
	}
	if body2.Token != body.Token {
		t.Fatal("shell token changed between calls")
	}

	// The shell token authenticates on /v1 (auth runs before the dead engine
	// call, so anything but 401 proves the token passed).
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:11470/v1/torrents", nil)
	req.Header.Set("Authorization", "Bearer "+body.Token)
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, req)
	if rec3.Code == http.StatusUnauthorized {
		t.Fatalf("shell token rejected on /v1: %s", rec3.Body.String())
	}

	// No token at all: 401.
	rec4 := httptest.NewRecorder()
	h.ServeHTTP(rec4, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:11470/v1/torrents", nil))
	if rec4.Code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d, want 401", rec4.Code)
	}
}

func TestShellTokenRejectsBrowserHeaders(t *testing.T) {
	h, store := shellTestHandler(t, true)

	// Allowlisted origin + cross-site passes the httpx perimeter; the handler
	// itself must still refuse.
	rec := postShellToken(t, h, map[string]string{
		"Origin":         "https://crispy.app",
		"Sec-Fetch-Site": "cross-site",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site browser: got %d, want 403", rec.Code)
	}
	// Any other Sec-Fetch-Site value is a browser too.
	rec = postShellToken(t, h, map[string]string{"Sec-Fetch-Site": "same-origin"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("same-origin: got %d, want 403", rec.Code)
	}
	// Origin alone (no Sec-Fetch-Site) is still a browser.
	rec = postShellToken(t, h, map[string]string{"Origin": "https://crispy.app"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("origin only: got %d, want 403", rec.Code)
	}
	if store.Count() != 0 {
		t.Fatal("browser-shaped requests minted a token")
	}
}
