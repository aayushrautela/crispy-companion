package server_test

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crispy-streaming/crispy-companion/internal/engine"
	"github.com/crispy-streaming/crispy-companion/internal/grant"
	"github.com/crispy-streaming/crispy-companion/internal/httpx"
	"github.com/crispy-streaming/crispy-companion/internal/notify"
	"github.com/crispy-streaming/crispy-companion/internal/pairing"
	"github.com/crispy-streaming/crispy-companion/internal/server"
)

// testHash is the infohash every /v1 test torrents under.
const testHash = "ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12"

// mintToken issues a paired-device token for the tests that need one.
func mintToken(t *testing.T, store *pairing.Store) string {
	t.Helper()
	tok, err := store.Mint("user-1", "alice@example.com", "dev-1")
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	return tok
}

// v1FakeTorrent is one engine-side torrent the fake engine holds in memory.
type v1FakeTorrent struct {
	Name      string
	Stat      int
	Files     []engine.FileStat
	PieceLen  int64
	Pieces    map[int]bool
	Speed     float64
	Active    int
	Seeders   int
	Upload    float64
	TotalSize int64
}

// v1FakeEngine serves the engine API surface the companion uses, with
// metadata, cache and Range support so the /v1 handlers can be driven end to
// end.
type v1FakeEngine struct {
	t        *testing.T
	mu       sync.Mutex
	torrents map[string]*v1FakeTorrent
	adds     int
	rems     int
	media    []byte
}

func newV1FakeEngine(t *testing.T) *v1FakeEngine {
	t.Helper()
	f := &v1FakeEngine{t: t, torrents: map[string]*v1FakeTorrent{}, media: make([]byte, 4000)}
	for i := range f.media {
		f.media[i] = byte('A' + i%26)
	}
	return f
}

func (f *v1FakeEngine) status(id string, tr *v1FakeTorrent) engine.TorrentStatus {
	st := engine.TorrentStatus{
		Name:             tr.Name,
		Hash:             id,
		Stat:             tr.Stat,
		StatString:       fmt.Sprintf("stat %d", tr.Stat),
		TorrentSize:      tr.TotalSize,
		DownloadSpeed:    tr.Speed,
		UploadSpeed:      tr.Upload,
		ActivePeers:      tr.Active,
		ConnectedSeeders: tr.Seeders,
		FileStats:        tr.Files,
	}
	return st
}

func (f *v1FakeEngine) cache(tr *v1FakeTorrent) engine.CacheState {
	cs := engine.CacheState{PiecesLength: tr.PieceLen}
	if len(tr.Pieces) == 0 {
		return cs
	}
	cs.Pieces = make(map[string]engine.PieceState, len(tr.Pieces))
	for p := range tr.Pieces {
		cs.Pieces[strconv.Itoa(p)] = engine.PieceState{Completed: true, Length: tr.PieceLen, Size: tr.PieceLen}
		cs.PiecesCount++
	}
	return cs
}

func (f *v1FakeEngine) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/torrents", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Action, Link, Hash string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		switch req.Action {
		case "add":
			if !strings.Contains(req.Link, testHash) && !strings.HasPrefix(req.Link, "magnet:") {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.adds++
			tr, ok := f.torrents[testHash]
			if !ok {
				tr = &v1FakeTorrent{}
				f.torrents[testHash] = tr
			}
			_ = json.NewEncoder(w).Encode(f.status(testHash, tr))
		case "get":
			tr, ok := f.torrents[req.Hash]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(f.status(req.Hash, tr))
		case "rem":
			f.rems++
			delete(f.torrents, req.Hash)
			w.WriteHeader(http.StatusOK)
		case "list":
			out := make([]engine.TorrentStatus, 0, len(f.torrents))
			for id, tr := range f.torrents {
				out = append(out, f.status(id, tr))
			}
			_ = json.NewEncoder(w).Encode(out)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	mux.HandleFunc("/cache", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Action, Hash string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		tr, ok := f.torrents[req.Hash]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(f.cache(tr))
	})
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		media := f.media
		_, loaded := f.torrents[r.URL.Query().Get("link")]
		f.mu.Unlock()
		if !loaded {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body := media[:1000]
		if r.URL.Query().Get("index") == "2" {
			body = media[1000:]
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("ETag", "fake")
		w.Header().Set("Accept-Ranges", "bytes")
		if h := r.Header.Get("Range"); h != "" {
			var start, end int
			if _, err := fmt.Sscanf(h, "bytes=%d-%d", &start, &end); err == nil {
				if start >= len(body) {
					w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
					return
				}
				if end >= len(body) {
					end = len(body) - 1
				}
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
				w.WriteHeader(http.StatusPartialContent)
				if r.Method != http.MethodHead {
					_, _ = w.Write(body[start : end+1])
				}
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
		}
	})
	return mux
}

func (f *v1FakeEngine) set(tr *v1FakeTorrent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.torrents[testHash] = tr
}

func (f *v1FakeEngine) drop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.torrents, testHash)
}

func (f *v1FakeEngine) counts() (adds, rems int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.adds, f.rems
}

// testV1Handler builds a companion with a small buffer floor so readiness is
// deterministic against the fake cache.
func testV1Handler(t *testing.T, engURL string, allowInternal bool) (http.Handler, *pairing.Store) {
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
	eng, err := engine.New(engine.Config{Addr: "127.0.0.1", Port: 18091})
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
		Notify:           notify.LogNotifier{Log: log.New(os.Stderr, "test: ", 0)},
		Mode:             "loopback",
		Grant:            grant.New(""),
		Session:          session,
		DeviceName:       "test device",
		Torrents:         engine.NewClient(engURL),
		Links:            engine.NewLinkStore(),
		PairingStatePath: filepath.Join(dir, "pairing-state.json"),
		Buffer: server.BufferConfig{
			MinBytes:                 8,
			StartSeconds:             10,
			BitrateWindowSeconds:     10,
			SustainableMarginPercent: 100,
		},
		ProxyAllowInternal: allowInternal,
	})
	return h, store
}

type v1Status struct {
	ID               string  `json:"id"`
	Name             *string `json:"name"`
	State            string  `json:"state"`
	Ready            bool    `json:"ready"`
	Progress         float64 `json:"progress"`
	MetadataProgress float64 `json:"metadataProgress"`
	FileIdx          int     `json:"fileIdx"`
	SelectionReason  string  `json:"selectionReason"`
	RetryAfterMs     int     `json:"retryAfterMs"`
	Buffer           struct {
		ContiguousBytes int64   `json:"contiguousBytes"`
		Seconds         float64 `json:"seconds"`
		Bitrate         int64   `json:"bitrate"`
		Sustainable     bool    `json:"sustainable"`
	} `json:"buffer"`
	Files []struct {
		Idx       int    `json:"idx"`
		Name      string `json:"name"`
		Size      int64  `json:"size"`
		VideoLike bool   `json:"videoLike"`
		Wanted    bool   `json:"wanted"`
	} `json:"files"`
	Error *struct {
		Code string `json:"code"`
	} `json:"error"`
}

func decodeStatus(t *testing.T, rec *httptest.ResponseRecorder) v1Status {
	t.Helper()
	var st v1Status
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode status %q: %v", rec.Body.String(), err)
	}
	return st
}

// TestV1Lifecycle walks the whole native surface: create, poll until ready,
// stream (full, range, HEAD), idle-drop recovery, delete, re-add, list.
func TestV1Lifecycle(t *testing.T) {
	fe := newV1FakeEngine(t)
	engSrv := httptest.NewServer(fe.handler())
	defer engSrv.Close()
	h, store := testV1Handler(t, engSrv.URL, false)
	tok := mintToken(t, store)
	hdrs := func() map[string]string {
		return map[string]string{
			"Origin":          "https://crispy.app",
			"Authorization":   "Bearer " + tok,
			"X-Crispy-Client": "crispy-web/1",
		}
	}
	base := "http://127.0.0.1:11470"

	// create
	body := fmt.Sprintf(`{"url":"magnet:?xt=urn:btih:%s"}`, testHash)
	req := httptest.NewRequest(http.MethodPost, base+"/v1/torrents", strings.NewReader(body))
	for k, v := range hdrs() {
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
		Ready bool   `json:"ready"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID != testHash || created.State != "metadata" || created.Ready {
		t.Fatalf("create body = %+v", created)
	}

	// pre-metadata poll
	rec = doReq(h, http.MethodGet, base+"/v1/torrents/"+testHash, hdrs())
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
	}
	st := decodeStatus(t, rec)
	if st.State != "metadata" || st.MetadataProgress != 0 || st.Ready || st.Name != nil {
		t.Fatalf("metadata status = %+v", st)
	}
	if st.RetryAfterMs != 500 {
		t.Fatalf("retryAfterMs = %d, want 500", st.RetryAfterMs)
	}

	// metadata arrives: two files, one video, downloading at 1 KiB/s.
	fe.set(&v1FakeTorrent{
		Name:      "multi",
		Stat:      3,
		Files:     []engine.FileStat{{ID: 1, Path: "a/b.mp4", Length: 1000}, {ID: 2, Path: "a/c.mp4", Length: 3000}},
		PieceLen:  1000,
		Speed:     1000,
		Active:    2,
		Seeders:   1,
		TotalSize: 4000,
	})
	rec = doReq(h, http.MethodGet, base+"/v1/torrents/"+testHash, hdrs())
	st = decodeStatus(t, rec)
	if st.State != "downloading" || st.MetadataProgress != 1 || st.Ready {
		t.Fatalf("downloading status = %+v", st)
	}
	if st.SelectionReason != "auto-largest" || st.FileIdx != 1 {
		t.Fatalf("selection = %d/%q, want 1/auto-largest", st.FileIdx, st.SelectionReason)
	}
	if len(st.Files) != 2 || !st.Files[1].Wanted || !st.Files[1].VideoLike || st.Files[0].Wanted {
		t.Fatalf("files = %+v", st.Files)
	}

	// first contiguous piece of the selected file: still short of 4 MiB floor,
	// but our test floor is 8 bytes, so one piece makes it ready.
	fe.set(&v1FakeTorrent{
		Name:      "multi",
		Stat:      3,
		Files:     []engine.FileStat{{ID: 1, Path: "a/b.mp4", Length: 1000}, {ID: 2, Path: "a/c.mp4", Length: 3000}},
		PieceLen:  1000,
		Pieces:    map[int]bool{1: true},
		Speed:     1000,
		Active:    2,
		Seeders:   1,
		TotalSize: 4000,
	})
	rec = doReq(h, http.MethodGet, base+"/v1/torrents/"+testHash, hdrs())
	st = decodeStatus(t, rec)
	if !st.Ready || st.Buffer.ContiguousBytes != 1000 {
		t.Fatalf("ready status = %+v", st)
	}
	if st.State != "downloading" {
		t.Fatalf("state with partial file = %q", st.State)
	}

	// file complete: seeding, always ready.
	fe.set(&v1FakeTorrent{
		Name:      "multi",
		Stat:      3,
		Files:     []engine.FileStat{{ID: 1, Path: "a/b.mp4", Length: 1000}, {ID: 2, Path: "a/c.mp4", Length: 3000}},
		PieceLen:  1000,
		Pieces:    map[int]bool{1: true, 2: true, 3: true},
		Active:    2,
		Seeders:   1,
		TotalSize: 4000,
	})
	rec = doReq(h, http.MethodGet, base+"/v1/torrents/"+testHash, hdrs())
	st = decodeStatus(t, rec)
	if st.State != "seeding" || st.Progress != 1 || !st.Ready {
		t.Fatalf("seeding status = %+v", st)
	}

	// stream: full body, range, HEAD — all through the relay.
	streamURL := fmt.Sprintf("%s/v1/torrents/%s/stream?file=1&token=%s", base, testHash, url.QueryEscape(tok))
	rec = doReq(h, http.MethodGet, streamURL, hdrs())
	if rec.Code != http.StatusOK || rec.Body.Len() != 3000 {
		t.Fatalf("stream: %d len %d", rec.Code, rec.Body.Len())
	}
	rng := hdrs()
	rng["Range"] = "bytes=100-199"
	rec = doReq(h, http.MethodGet, streamURL, rng)
	if rec.Code != http.StatusPartialContent || rec.Body.Len() != 100 {
		t.Fatalf("range stream: %d len %d", rec.Code, rec.Body.Len())
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 100-199/3000" {
		t.Fatalf("Content-Range = %q", got)
	}
	rec = doReq(h, http.MethodHead, streamURL, hdrs())
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("HEAD stream: %d len %d", rec.Code, rec.Body.Len())
	}

	// idle drop: the engine forgets the torrent, the companion re-adds it.
	addsBefore, _ := fe.counts()
	fe.drop()
	rec = doReq(h, http.MethodGet, base+"/v1/torrents/"+testHash, hdrs())
	if rec.Code != http.StatusOK {
		t.Fatalf("status after drop: %d %s", rec.Code, rec.Body.String())
	}
	addsAfter, _ := fe.counts()
	if addsAfter != addsBefore+1 {
		t.Fatalf("re-adds = %d, want %d", addsAfter, addsBefore+1)
	}

	// delete clears engine state and the companion's link store.
	rec = doReq(h, http.MethodDelete, base+"/v1/torrents/"+testHash, hdrs())
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if _, rems := fe.counts(); rems != 1 {
		t.Fatalf("rems = %d, want 1", rems)
	}
	rec = doReq(h, http.MethodGet, base+"/v1/torrents/"+testHash, hdrs())
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status after delete: %d %s", rec.Code, rec.Body.String())
	}

	// re-create is idempotent, and the list reflects it.
	req = httptest.NewRequest(http.MethodPost, base+"/v1/torrents", strings.NewReader(body))
	for k, v := range hdrs() {
		req.Header.Set(k, v)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("re-create: %d %s", rec.Code, rec.Body.String())
	}
	rec = doReq(h, http.MethodGet, base+"/v1/torrents", hdrs())
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Torrents []struct {
			ID string `json:"id"`
		} `json:"torrents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Torrents) != 1 || list.Torrents[0].ID != testHash {
		t.Fatalf("list = %+v", list)
	}
}

// TestV1Proxy covers the pass-through endpoint: target streaming, Range, 5xx
// passthrough, and the address guard.
func TestV1Proxy(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/data":
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Accept-Ranges", "bytes")
			if r.Header.Get("Range") != "" {
				w.Header().Set("Content-Range", "bytes 0-4/5")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write([]byte("part="))
				return
			}
			_, _ = w.Write([]byte("hello"))
		case "/fail":
			w.WriteHeader(http.StatusBadGateway)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer target.Close()

	fe := newV1FakeEngine(t)
	engSrv := httptest.NewServer(fe.handler())
	defer engSrv.Close()
	h, store := testV1Handler(t, engSrv.URL, true)
	tok := mintToken(t, store)
	hdrs := map[string]string{
		"Origin":          "https://crispy.app",
		"Authorization":   "Bearer " + tok,
		"X-Crispy-Client": "crispy-web/1",
	}
	base := "http://127.0.0.1:11470/v1/proxy?token=" + url.QueryEscape(tok)

	rec := doReq(h, http.MethodGet, base+"&target="+url.QueryEscape(target.URL+"/data"), hdrs)
	if rec.Code != http.StatusOK || rec.Body.String() != "hello" {
		t.Fatalf("proxy: %d %q", rec.Code, rec.Body.String())
	}
	rng := map[string]string{}
	for k, v := range hdrs {
		rng[k] = v
	}
	rng["Range"] = "bytes=0-4"
	rec = doReq(h, http.MethodGet, base+"&target="+url.QueryEscape(target.URL+"/data"), rng)
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "part=" {
		t.Fatalf("proxy range: %d %q", rec.Code, rec.Body.String())
	}
	rec = doReq(h, http.MethodGet, base+"&target="+url.QueryEscape(target.URL+"/fail"), hdrs)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("proxy 5xx passthrough: %d", rec.Code)
	}
	rec = doReq(h, http.MethodGet, base, hdrs)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("proxy without target: %d", rec.Code)
	}
	rec = doReq(h, http.MethodGet, base+"&target="+url.QueryEscape("ftp://example.com/x"), hdrs)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("proxy bad scheme: %d", rec.Code)
	}

	// With the guard on, a loopback target is refused before any dial.
	strict, strictStore := testV1Handler(t, engSrv.URL, false)
	strictHdrs := map[string]string{
		"Origin":          "https://crispy.app",
		"Authorization":   "Bearer " + mintToken(t, strictStore),
		"X-Crispy-Client": "crispy-web/1",
	}
	rec = doReq(strict, http.MethodGet, "http://127.0.0.1:11470/v1/proxy?token="+
		url.QueryEscape(strictHdrs["Authorization"])+"&target="+url.QueryEscape(target.URL+"/data"), strictHdrs)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("proxy loopback target: %d %s", rec.Code, rec.Body.String())
	}
}

// TestV1AuthAndPerimeter pins the token and client-header rules and the
// single-error envelope.
func TestV1AuthAndPerimeter(t *testing.T) {
	fe := newV1FakeEngine(t)
	engSrv := httptest.NewServer(fe.handler())
	defer engSrv.Close()
	h, store := testV1Handler(t, engSrv.URL, false)
	tok := mintToken(t, store)
	base := "http://127.0.0.1:11470"
	magnet := fmt.Sprintf(`{"url":"magnet:?xt=urn:btih:%s"}`, testHash)

	authHdrs := func() map[string]string {
		return map[string]string{
			"Origin":          "https://crispy.app",
			"Authorization":   "Bearer " + tok,
			"X-Crispy-Client": "crispy-web/1",
		}
	}

	rec := doReq(h, http.MethodGet, base+"/v1/torrents", map[string]string{"Origin": "https://crispy.app"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("no token body = %q", rec.Body.String())
	}

	bad := map[string]string{
		"Origin":        "https://crispy.app",
		"Authorization": "Bearer nope",
	}
	rec = doReq(h, http.MethodGet, base+"/v1/torrents", bad)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", rec.Code)
	}

	post := func(hdrs map[string]string, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, base+"/v1/torrents", strings.NewReader(body))
		for k, v := range hdrs {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	noClient := map[string]string{"Origin": "https://crispy.app", "Authorization": "Bearer " + tok}
	if rec := post(noClient, magnet); rec.Code != http.StatusBadRequest {
		t.Fatalf("post without X-Crispy-Client: %d", rec.Code)
	}
	if rec := post(authHdrs(), `{"url":"file:///etc/passwd"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("post bad link: %d %s", rec.Code, rec.Body.String())
	}
	rec = doReq(h, http.MethodDelete, base+"/v1/torrents/"+testHash, noClient)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("delete without X-Crispy-Client: %d", rec.Code)
	}
	rec = doReq(h, http.MethodGet, base+"/v1/torrents/not-a-hash", authHdrs())
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bad id: %d", rec.Code)
	}
}

// TestV1RateLimit checks the create budget: ten per token per minute.
func TestV1RateLimit(t *testing.T) {
	fe := newV1FakeEngine(t)
	engSrv := httptest.NewServer(fe.handler())
	defer engSrv.Close()
	h, store := testV1Handler(t, engSrv.URL, false)
	tok := mintToken(t, store)
	base := "http://127.0.0.1:11470"
	body := fmt.Sprintf(`{"url":"magnet:?xt=urn:btih:%s"}`, testHash)
	for i := 1; i <= 10; i++ {
		req := httptest.NewRequest(http.MethodPost, base+"/v1/torrents", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("X-Crispy-Client", "crispy-web/1")
		req.Header.Set("Origin", "https://crispy.app")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("create %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	req := httptest.NewRequest(http.MethodPost, base+"/v1/torrents", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-Crispy-Client", "crispy-web/1")
	req.Header.Set("Origin", "https://crispy.app")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("11th create: %d %s", rec.Code, rec.Body.String())
	}
}
