package server

import (
	"context"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/crispy-streaming/crispy-companion/internal/engine"
)

// maxMediaProxies caps concurrent proxied media streams (PLAN §9:
// shared.maxConcurrentMediaProxies, default 32).
const maxMediaProxies = 32

// normalizeHash lowercases hex hashes and converts base32 hashes to hex,
// the form the engine requires. Rejects anything unparseable.
func normalizeHash(h string) (string, error) {
	if len(h) == 40 {
		return strings.ToLower(h), nil
	}
	if len(h) == 32 {
		raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(h))
		if err != nil || len(raw) != 20 {
			return "", fmt.Errorf("bad base32 infohash")
		}
		const hexd = "0123456789abcdef"
		out := make([]byte, 40)
		for i, b := range raw {
			out[2*i] = hexd[b>>4]
			out[2*i+1] = hexd[b&0xf]
		}
		return string(out), nil
	}
	return "", fmt.Errorf("bad infohash length")
}

// handleStreamCompat routes the Stremio-compatible streaming surface:
// POST /{ih}/create, GET /{ih}/{idx}/stats.json, GET|HEAD /{ih}/{idx}.
func (d Deps) handleStreamCompat(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/")
	seg, tail, _ := strings.Cut(rest, "/")
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
	hash, err := normalizeHash(seg)
	if err != nil {
		http.Error(w, "bad infohash", http.StatusBadRequest)
		return
	}

	switch {
	case tail == "create" && r.Method == http.MethodPost:
		d.handleCreate(w, r, hash)
		return
	case strings.HasSuffix(tail, "/stats.json") && r.Method == http.MethodGet:
		idxStr := strings.TrimSuffix(tail, "/stats.json")
		d.handleStats(w, r, hash, idxStr)
		return
	case (r.Method == http.MethodGet || r.Method == http.MethodHead) && isFileIdx(tail):
		d.handleMedia(w, r, hash, tail)
		return
	}
	http.NotFound(w, r)
}

func isFileIdx(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// handleCreate adds the torrent to the engine (idempotent: re-adds a dropped
// one). Accepts an optional JSON body {link} with a full magnet, raw hash,
// or http(s) .torrent URL; otherwise synthesizes a tracker-enriched magnet.
func (d Deps) handleCreate(w http.ResponseWriter, r *http.Request, hash string) {
	if !requireClientHeader(w, r) {
		return
	}
	if d.Torrents == nil {
		http.Error(w, "engine unavailable", http.StatusServiceUnavailable)
		return
	}
	link := createLink(r, hash)
	d.Links.Set(hash, link)
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	st, err := d.Torrents.Add(ctx, link)
	if err != nil {
		http.Error(w, "engine unavailable", http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "created",
		"infoHash": st.Hash,
	})
}

// createLink picks the add link: validated client link, else synthesized.
func createLink(r *http.Request, hash string) string {
	if r.Body != nil {
		body, err := io.ReadAll(io.LimitReader(r.Body, 4097))
		if err == nil && len(body) > 0 {
			var v struct {
				Link string `json:"link"`
			}
			if json.Unmarshal(body, &v) == nil {
				if l := sanitizeLink(strings.TrimSpace(v.Link)); l != "" {
					return l
				}
			}
		}
	}
	return engine.MagnetForHash(hash)
}

// sanitizeLink accepts magnets, raw 40-hex hashes, and http(s) .torrent
// URLs. Everything else (file://, torrs://, javascript:, …) is rejected:
// the link is handed to the engine's parser, so the allowlist is tight.
func sanitizeLink(l string) string {
	if l == "" || len(l) > 4096 {
		return ""
	}
	lower := strings.ToLower(l)
	switch {
	case strings.HasPrefix(lower, "magnet:?"):
		return l
	case strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://"):
		return l
	}
	if isInfoHash(l) && len(l) == 40 {
		return strings.ToLower(l)
	}
	return ""
}

// resolve returns the engine status, transparently re-adding a torrent the
// engine dropped after its idle timeout (verified M0 behavior: zero readers
// + 30s default → 404 on get/cache).
func (d Deps) resolve(ctx context.Context, hash string) (engine.TorrentStatus, error) {
	st, err := d.Torrents.Get(ctx, hash)
	if err != engine.ErrNotFound {
		return st, err
	}
	link, ok := d.Links.Get(hash)
	if !ok {
		link = engine.MagnetForHash(hash)
		d.Links.Set(hash, link)
	}
	if _, err := d.Torrents.Add(ctx, link); err != nil {
		return engine.TorrentStatus{}, err
	}
	return d.Torrents.Get(ctx, hash)
}

type statsResponse struct {
	StreamProgress float64 `json:"streamProgress"`
	StreamLen      int64   `json:"streamLen"`
	StreamName     string  `json:"streamName"`
	Downloaded     int64   `json:"downloaded"`
	PieceLength    int64   `json:"pieceLength"`
	EngineFileID   int     `json:"engineFileId"`
}

// handleStats reports per-file buffer progress (the webapp's 5% ready gate
// consumes streamProgress unchanged from the Stremio flow).
func (d Deps) handleStats(w http.ResponseWriter, r *http.Request, hash, idxStr string) {
	if d.Torrents == nil {
		http.Error(w, "engine unavailable", http.StatusServiceUnavailable)
		return
	}
	idx, err := strconv.Atoi(idxStr)
	if err != nil || idx < 0 {
		http.Error(w, "bad file index", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	st, err := d.resolve(ctx, hash)
	if err != nil {
		if err == engine.ErrNotFound {
			http.Error(w, "torrent not ready", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "engine unavailable", http.StatusBadGateway)
		return
	}
	file, err := engine.FileByIndex(st, idx)
	if err != nil {
		if len(st.FileStats) == 0 {
			http.Error(w, "torrent not ready", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	resp := statsResponse{
		StreamLen:    file.Length,
		StreamName:   mediaName(file.Path),
		EngineFileID: file.ID,
	}
	if cs, err := d.Torrents.Cache(ctx, hash); err == nil {
		resp.PieceLength = cs.PiecesLength
		resp.StreamProgress = engine.ProgressForFile(st, cs, file)
		resp.Downloaded = engine.DownloadedForFile(st, cs, file)
	}
	writeJSON(w, http.StatusOK, resp)
}

// mediaName is the display name: last path segment of the engine file path.
func mediaName(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// handleMedia proxies engine bytes with Range passthrough (the CORS fix:
// the browser never touches TorrServer URLs). No buffering, no
// transformation; client aborts propagate to the engine via context.
func (d Deps) handleMedia(w http.ResponseWriter, r *http.Request, hash, idxStr string) {
	if d.Torrents == nil {
		http.Error(w, "engine unavailable", http.StatusServiceUnavailable)
		return
	}
	idx, err := strconv.Atoi(idxStr)
	if err != nil || idx < 0 {
		http.Error(w, "bad file index", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	st, err := d.resolve(ctx, hash)
	if err != nil {
		if err == engine.ErrNotFound {
			http.Error(w, "torrent not ready", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "engine unavailable", http.StatusBadGateway)
		return
	}
	file, err := engine.FileByIndex(st, idx)
	if err != nil {
		if len(st.FileStats) == 0 {
			http.Error(w, "torrent not ready", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	select {
	case d.MediaSem <- struct{}{}:
		defer func() { <-d.MediaSem }()
	default:
		http.Error(w, "too many streams", http.StatusServiceUnavailable)
		return
	}
	upReq, err := http.NewRequestWithContext(ctx, r.Method, d.Torrents.StreamURL(hash, file.ID), nil)
	if err != nil {
		http.Error(w, "engine unavailable", http.StatusBadGateway)
		return
	}
	if h := r.Header.Get("Range"); h != "" {
		upReq.Header.Set("Range", h)
	}
	if h := r.Header.Get("If-Range"); h != "" {
		upReq.Header.Set("If-Range", h)
	}
	upReq.Header.Set("User-Agent", "crispy-companion/1")
	upResp, err := d.Torrents.HTTP.Do(upReq)
	if err != nil {
		http.Error(w, "engine unavailable", http.StatusBadGateway)
		return
	}
	defer upResp.Body.Close()
	for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"} {
		if v := upResp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.WriteHeader(upResp.StatusCode)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, upResp.Body)
	}
}
