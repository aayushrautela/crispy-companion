package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crispy-streaming/crispy-companion/internal/engine"
	"github.com/crispy-streaming/crispy-companion/internal/grant"
	"github.com/crispy-streaming/crispy-companion/internal/httpx"
	"github.com/crispy-streaming/crispy-companion/internal/notify"
	"github.com/crispy-streaming/crispy-companion/internal/pairing"
	"github.com/crispy-streaming/crispy-companion/internal/server"
)

// liveTorrentHash is the Ubuntu ISO used by the engine spike (single file,
// ~6.3 GB). Override with CRISPY_LIVE_MAGNET when testing another torrent.
const liveTorrentHash = "d160b8d8ea35a5b4e52837468fc8f03d55cef1f7"

// TestLiveEngineV1 drives the native surface against a real TorrServer.
//
// Opt-in, needs an engine on loopback:
//
//	CRISPY_LIVE_ENGINE=http://127.0.0.1:8090 go test ./internal/server/ -run TestLiveEngineV1 -v
func TestLiveEngineV1(t *testing.T) {
	engineURL := os.Getenv("CRISPY_LIVE_ENGINE")
	if engineURL == "" {
		t.Skip("set CRISPY_LIVE_ENGINE to run against a live engine")
	}
	magnet := os.Getenv("CRISPY_LIVE_MAGNET")
	if magnet == "" {
		magnet = engine.MagnetForHash(liveTorrentHash)
	}

	dir := t.TempDir()
	store, err := pairing.Open(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := pairing.OpenSession(filepath.Join(dir, "device-session.json"))
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(engine.Config{Addr: "127.0.0.1", Port: 18099})
	if err != nil {
		t.Fatal(err)
	}
	// Bind first so the perimeter's Host allowlist can name the real port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h := server.New(server.Deps{
		Policy: httpx.NewPolicy(
			[]string{ln.Addr().String(), "127.0.0.1:11470", "localhost:11470"},
			[]string{"https://crispy.app"}, false),
		Tokens:           store,
		Window:           pairing.NewWindow(90*time.Second, 5),
		Engine:           eng,
		Notify:           notify.LogNotifier{Log: log.New(os.Stderr, "live: ", 0)},
		Mode:             "loopback",
		Grant:            grant.New(""),
		Session:          session,
		DeviceName:       "live test",
		Torrents:         engine.NewClient(engineURL),
		Links:            engine.NewLinkStore(),
		PairingStatePath: filepath.Join(dir, "pairing-state.json"),
		Buffer: server.BufferConfig{
			MinBytes:                 64 << 10,
			StartSeconds:             2,
			BitrateWindowSeconds:     5,
			SustainableMarginPercent: 100,
		},
	})
	tok := mintToken(t, store)
	hdrs := map[string]string{
		"Origin":          "https://crispy.app",
		"Authorization":   "Bearer " + tok,
		"X-Crispy-Client": "crispy-web/1",
	}
	// A real listener so the stream request below is a genuine open reader:
	// the engine only fetches data for attached readers, so readiness can
	// never flip while nothing is streaming.
	srv := httptest.NewUnstartedServer(h)
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	defer srv.Close()
	base := srv.URL

	req := httptest.NewRequest(http.MethodPost, base+"/v1/torrents",
		strings.NewReader(fmt.Sprintf(`{"url":%q,"fileIdx":0}`, magnet)))
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if len(created.ID) != 40 {
		t.Fatalf("create id = %q", created.ID)
	}
	t.Logf("create → id=%s state=%s", created.ID, created.State)
	defer func() {
		rec := doReq(h, http.MethodDelete, base+"/v1/torrents/"+created.ID, hdrs)
		t.Logf("delete → %d", rec.Code)
	}()

	rec = doReq(h, http.MethodGet, base+"/v1/torrents", hdrs)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	t.Logf("list → %s", rec.Body.String())

	// Metadata first: until the engine reports a file list there is nothing
	// to address a read against (the stream route answers 404 by design).
	metaDeadline := time.Now().Add(90 * time.Second)
	var meta v1Status
	for time.Now().Before(metaDeadline) {
		rec = doReq(h, http.MethodGet, base+"/v1/torrents/"+created.ID, hdrs)
		if rec.Code != http.StatusOK {
			t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
			t.Fatal(err)
		}
		if len(meta.Files) > 0 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if len(meta.Files) == 0 {
		t.Fatalf("metadata never arrived: %s", rec.Body.String())
	}
	f := meta.Files[meta.FileIdx]
	t.Logf("metadata → state=%s fileIdx=%d file=%s (%d bytes, videoLike=%v)",
		meta.State, f.Idx, f.Name, f.Size, f.VideoLike)

	// The engine is streaming-first: it only pulls data for attached readers.
	// So open a real stream request (Range 0-3 MiB) and keep it open while we
	// poll — that is exactly the order the webapp follows (start reading, then
	// report buffer progress), and it is the only way `ready` can flip.
	streamURL := fmt.Sprintf("%s/v1/torrents/%s/stream?file=0&token=%s",
		base, created.ID, url.QueryEscape(tok))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	streamReq, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	streamReq.Header.Set("Origin", "https://crispy.app")
	streamReq.Header.Set("Range", "bytes=0-3145727")
	streamResp, err := http.DefaultClient.Do(streamReq)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer streamResp.Body.Close()
	t.Logf("stream open → %d content-length=%q", streamResp.StatusCode, streamResp.Header.Get("Content-Length"))
	if streamResp.StatusCode != http.StatusPartialContent && streamResp.StatusCode != http.StatusOK {
		t.Fatalf("open stream: %d", streamResp.StatusCode)
	}

	deadline := time.Now().Add(120 * time.Second)
	var last v1Status
	ready := false
	for time.Now().Before(deadline) {
		rec = doReq(h, http.MethodGet, base+"/v1/torrents/"+created.ID, hdrs)
		if rec.Code != http.StatusOK {
			t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &last); err != nil {
			t.Fatal(err)
		}
		if last.Ready {
			ready = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("status → state=%s ready=%v progress=%.5f contiguous=%d seconds=%.1f bitrate=%d sustainable=%v files=%d",
		last.State, last.Ready, last.Progress, last.Buffer.ContiguousBytes, last.Buffer.Seconds,
		last.Buffer.Bitrate, last.Buffer.Sustainable, len(last.Files))
	if len(last.Files) > 0 && last.FileIdx >= 0 {
		f := last.Files[last.FileIdx]
		t.Logf("selected file %d: %s (%d bytes, videoLike=%v)", f.Idx, f.Name, f.Size, f.VideoLike)
	}
	if !ready {
		t.Fatalf("never became ready while streaming: state=%s contiguous=%d", last.State, last.Buffer.ContiguousBytes)
	}
	if last.Buffer.ContiguousBytes < 64<<10 {
		t.Fatalf("ready with only %d contiguous bytes", last.Buffer.ContiguousBytes)
	}
	if len(last.Files) == 0 {
		t.Fatal("ready without a file list")
	}
}
