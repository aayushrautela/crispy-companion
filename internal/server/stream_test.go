package server_test

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crispy-streaming/crispy-companion/internal/engine"
	"github.com/crispy-streaming/crispy-companion/internal/grant"
	"github.com/crispy-streaming/crispy-companion/internal/httpx"
	"github.com/crispy-streaming/crispy-companion/internal/notify"
	"github.com/crispy-streaming/crispy-companion/internal/pairing"
	"github.com/crispy-streaming/crispy-companion/internal/server"
)

// fakeEngine mimics the TorrServer subset the shim uses, with knobs for
// drop simulation (get/cache 404 until re-added) and Range support.
type fakeEngine struct {
	t         *testing.T
	adds      int64
	dropFirst bool // 404 the first-ever get per hash (idle-drop simulation)
	dropped   map[string]bool
	got       map[string]int
	media     []byte
}

func (f *fakeEngine) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/torrents":
			var body struct {
				Action string `json:"action"`
				Link   string `json:"link"`
				Hash   string `json:"hash"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			hash := body.Hash
			if body.Action == "add" {
				atomic.AddInt64(&f.adds, 1)
				hash = "ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12"
				if !strings.Contains(body.Link, "ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12") &&
					!strings.Contains(strings.ToLower(body.Link), "magnet") {
					w.WriteHeader(400)
					_, _ = w.Write([]byte(`{"error":"bad link"}`))
					return
				}
				f.got[hash] = 0
			}
			if f.dropFirst && body.Action == "get" && !f.dropped[hash] {
				f.dropped[hash] = true
				w.WriteHeader(404)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": "multi", "hash": hash, "stat": 3, "stat_string": "Torrent working",
				"torrent_size": 4000,
				"file_stats": []map[string]any{
					{"id": 1, "path": "a/b.mp4", "length": 1000},
					{"id": 2, "path": "a/c.mp4", "length": 3000},
				},
			})
		case "/cache":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"PiecesLength": 1000, "PiecesCount": 4,
				"Pieces": map[string]any{
					"0": map[string]any{"Completed": true, "Length": 1000, "Size": 1000},
					"1": map[string]any{"Completed": true, "Length": 1000, "Size": 1000},
				},
			})
		case "/stream":
			idx := r.URL.Query().Get("index")
			length := 1000
			if idx == "2" {
				length = 3000
			}
			data := f.media[:length]
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("ETag", `"fake"`)
			if rg := r.Header.Get("Range"); rg != "" {
				var start, end int
				_, _ = fmt.Sscanf(rg, "bytes=%d-%d", &start, &end)
				if start >= len(data) {
					w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
					return
				}
				if end <= 0 || end >= len(data) {
					end = len(data) - 1
				}
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
				w.Header().Set("Accept-Ranges", "bytes")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(data[start : end+1])
				return
			}
			w.Header().Set("Accept-Ranges", "bytes")
			_, _ = w.Write(data)
		default:
			w.WriteHeader(404)
		}
	})
}

func testHandlerWithEngine(t *testing.T, engURL string) (http.Handler, *pairing.Store) {
	t.Helper()
	store, err := pairing.Open(filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := pairing.OpenSession(filepath.Join(t.TempDir(), "device-session.json"))
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(engine.Config{Addr: "127.0.0.1", Port: 18091})
	if err != nil {
		t.Fatal(err)
	}
	h := server.New(server.Deps{
		Policy: httpx.NewPolicy(
			[]string{"127.0.0.1:11470", "localhost:11470"},
			[]string{"https://crispy.app"},
			false,
		),
		Tokens:     store,
		Window:     pairing.NewWindow(90*time.Second, 5),
		Engine:     eng,
		Notify:     notify.LogNotifier{Log: log.New(os.Stderr, "test: ", 0)},
		Mode:       "loopback",
		Grant:      grant.New(""),
		Session:    session,
		DeviceName: "test device",
		Torrents:   engine.NewClient(engURL),
		Links:      engine.NewLinkStore(),
	})
	return h, store
}

const testHash = "ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12"

func mintToken(t *testing.T, store *pairing.Store) string {
	t.Helper()
	tok, err := store.Mint("user-1", "alice@example.com", "dev-1")
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestStreamCreateStatsMedia(t *testing.T) {
	media := make([]byte, 3000)
	for i := range media {
		media[i] = byte(i % 251)
	}
	fe := &fakeEngine{t: t, dropped: map[string]bool{}, got: map[string]int{}, media: media}
	srv := httptest.NewServer(fe.handler())
	defer srv.Close()
	h, store := testHandlerWithEngine(t, srv.URL)
	tok := mintToken(t, store)
	hdrs := func(extra map[string]string) map[string]string {
		m := map[string]string{"Origin": "https://crispy.app", "Authorization": "Bearer " + tok, "X-Crispy-Client": "crispy-web/1"}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}

	// create (hash-only body) → 200 created.
	rec := doReq(h, http.MethodPost, "http://127.0.0.1:11470/"+testHash+"/create", hdrs(nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("create: got %d (%s)", rec.Code, rec.Body.String())
	}
	var created map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil || created["status"] != "created" {
		t.Fatalf("bad create response: %v %s", err, rec.Body.String())
	}

	// stats file 0: piece 0 complete → progress 1.
	rec = doReq(h, http.MethodGet, "http://127.0.0.1:11470/"+testHash+"/0/stats.json",
		map[string]string{"Origin": "https://crispy.app", "Authorization": "Bearer " + tok})
	if rec.Code != http.StatusOK {
		t.Fatalf("stats: got %d (%s)", rec.Code, rec.Body.String())
	}
	var st struct {
		StreamProgress float64 `json:"streamProgress"`
		StreamLen      int64   `json:"streamLen"`
		StreamName     string  `json:"streamName"`
		Downloaded     int64   `json:"downloaded"`
		PieceLength    int64   `json:"pieceLength"`
		EngineFileID   int     `json:"engineFileId"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.StreamProgress != 1 || st.StreamLen != 1000 || st.StreamName != "b.mp4" ||
		st.Downloaded != 1000 || st.PieceLength != 1000 || st.EngineFileID != 1 {
		t.Fatalf("bad stats: %+v", st)
	}

	// stats file 1: pieces 1..3, only piece 1 complete → ~1/3.
	rec = doReq(h, http.MethodGet, "http://127.0.0.1:11470/"+testHash+"/1/stats.json",
		map[string]string{"Origin": "https://crispy.app", "Authorization": "Bearer " + tok})
	var st1 struct {
		StreamProgress float64 `json:"streamProgress"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&st1)
	if st1.StreamProgress < 0.333 || st1.StreamProgress > 0.334 {
		t.Fatalf("file 1 progress = %v", st1.StreamProgress)
	}

	// full media via query token (the <video> path): 200 passthrough.
	rec = doReq(h, http.MethodGet, "http://127.0.0.1:11470/"+testHash+"/0?token="+tok,
		map[string]string{"Origin": "https://crispy.app"})
	if rec.Code != http.StatusOK {
		t.Fatalf("media: got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "video/mp4" {
		t.Fatalf("content-type = %q", ct)
	}
	if rec.Body.Len() != 1000 {
		t.Fatalf("media bytes = %d, want 1000", rec.Body.Len())
	}
	for i, b := range rec.Body.Bytes() {
		if b != byte(i%251) {
			t.Fatalf("media byte %d corrupt", i)
		}
	}

	// Range passthrough → 206 with Content-Range.
	rec = doReq(h, http.MethodGet, "http://127.0.0.1:11470/"+testHash+"/0?token="+tok,
		map[string]string{"Origin": "https://crispy.app", "Range": "bytes=100-199"})
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("range: got %d", rec.Code)
	}
	if cr := rec.Header().Get("Content-Range"); cr != "bytes 100-199/1000" {
		t.Fatalf("content-range = %q", cr)
	}
	if rec.Body.Len() != 100 || rec.Body.Bytes()[0] != byte(100%251) {
		t.Fatalf("range bytes wrong: len=%d first=%d", rec.Body.Len(), rec.Body.Bytes()[0])
	}

	// HEAD: headers, no body.
	rec = doReq(h, http.MethodHead, "http://127.0.0.1:11470/"+testHash+"/0?token="+tok,
		map[string]string{"Origin": "https://crispy.app"})
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("head: got %d body=%d", rec.Code, rec.Body.Len())
	}

	// Out-of-range file → 404.
	rec = doReq(h, http.MethodGet, "http://127.0.0.1:11470/"+testHash+"/7/stats.json",
		map[string]string{"Origin": "https://crispy.app", "Authorization": "Bearer " + tok})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bad idx: got %d, want 404", rec.Code)
	}
}

func TestStreamReaddAfterIdleDrop(t *testing.T) {
	media := make([]byte, 1000)
	fe := &fakeEngine{t: t, dropped: map[string]bool{}, got: map[string]int{}, media: media, dropFirst: true}
	srv := httptest.NewServer(fe.handler())
	defer srv.Close()
	h, store := testHandlerWithEngine(t, srv.URL)
	tok := mintToken(t, store)

	// create records the link; the first get 404s (idle drop) and the shim
	// must re-add once, then serve stats.
	rec := doReq(h, http.MethodPost, "http://127.0.0.1:11470/"+testHash+"/create",
		map[string]string{"Origin": "https://crispy.app", "Authorization": "Bearer " + tok, "X-Crispy-Client": "crispy-web/1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("create: got %d", rec.Code)
	}
	addsBefore := atomic.LoadInt64(&fe.adds)
	rec = doReq(h, http.MethodGet, "http://127.0.0.1:11470/"+testHash+"/0/stats.json",
		map[string]string{"Origin": "https://crispy.app", "Authorization": "Bearer " + tok})
	if rec.Code != http.StatusOK {
		t.Fatalf("stats after drop: got %d (%s)", rec.Code, rec.Body.String())
	}
	if atomic.LoadInt64(&fe.adds) != addsBefore+1 {
		t.Fatalf("expected exactly one re-add, adds=%d", fe.adds)
	}
}

func TestStreamRejectsBadLinks(t *testing.T) {
	fe := &fakeEngine{t: t, dropped: map[string]bool{}, got: map[string]int{}, media: make([]byte, 10)}
	srv := httptest.NewServer(fe.handler())
	defer srv.Close()
	h, store := testHandlerWithEngine(t, srv.URL)
	tok := mintToken(t, store)
	for _, link := range []string{"file:///etc/passwd", "javascript:alert(1)", "torrs://abc", "ftp://x/y"} {
		body := `{"link":` + strconv.Quote(link) + `}`
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:11470/"+testHash+"/create", strings.NewReader(body))
		req.Header.Set("Origin", "https://crispy.app")
		req.Header.Set("X-Crispy-Client", "crispy-web/1")
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("link %q: got %d", link, rec.Code)
		}
	}
	// None of the bad links may have reached the engine: synthesized magnet
	// was used instead (adds count only, no 400s from fake).
	if atomic.LoadInt64(&fe.adds) != 4 {
		t.Fatalf("adds=%d, want 4 (all synthesized)", fe.adds)
	}
}
