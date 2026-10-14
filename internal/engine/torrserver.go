// Package engine speaks TorrServer's HTTP API on loopback. The engine is
// never exposed off-box; this client is the shim's only path to it.
//
// Verified against TorrServer MatriX.146.1 (see M0 spike notes):
//
//	POST /torrents {action:add,link} → TorrentStatus (link: magnet, raw
//	  hash, or http(s) .torrent URL; 40-hex hashes only)
//	POST /torrents {action:get,hash}  → TorrentStatus or 404 (dropped)
//	POST /cache    {action:get,hash}  → CacheState or 404 (dropped)
//	GET  /stream?link=<hash>&index=<1-based>&play → media bytes, Range via
//	  ServeContent. link accepts the raw hash; existing torrents are reused.
//	File IDs are 1-based over the path-sorted file list (Id 0 undefined).
//	Idle torrents (zero readers) are dropped after TorrentDisconnectTimeout
//	(30s default): callers must re-add transparently (see LinkStore).
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrNotFound means the engine has no such torrent (never added or dropped
// after the idle timeout). Re-add and retry.
var ErrNotFound = errors.New("torrent not found on engine")

// DefaultTrackers enrich hash-only magnets so metadata/peer discovery does
// not depend on DHT alone. Keep this list short and public.
var DefaultTrackers = []string{
	"udp://tracker.opentrackr.org:1337/announce",
	"udp://open.stealth.si:80/announce",
}

// MagnetForHash synthesizes a magnet for a bare infohash (tracker-enriched).
func MagnetForHash(infoHash string) string {
	var b strings.Builder
	b.WriteString("magnet:?xt=urn:btih:")
	b.WriteString(strings.ToLower(infoHash))
	for _, tr := range DefaultTrackers {
		b.WriteString("&tr=")
		b.WriteString(url.QueryEscape(tr))
	}
	return b.String()
}

// FileStat is one engine file entry (1-based Id, path-sorted).
type FileStat struct {
	ID     int    `json:"id"`
	Path   string `json:"path"`
	Length int64  `json:"length"`
}

// TorrentStatus is the subset of the engine status the shim needs.
type TorrentStatus struct {
	Name        string     `json:"name"`
	Hash        string     `json:"hash"`
	Stat        int        `json:"stat"`
	StatString  string     `json:"stat_string"`
	TorrentSize int64      `json:"torrent_size"`
	FileStats   []FileStat `json:"file_stats"`
}

// PieceState is one cache piece entry.
type PieceState struct {
	Completed bool  `json:"Completed"`
	Length    int64 `json:"Length"`
	Size      int64 `json:"Size"`
}

// CacheState is the subset of the engine cache state the shim needs.
type CacheState struct {
	PiecesLength int64                 `json:"PiecesLength"`
	PiecesCount  int                   `json:"PiecesCount"`
	Pieces       map[string]PieceState `json:"Pieces"`
}

// Client talks to TorrServer. HTTP should carry a Timeout.
type Client struct {
	BaseURL string // e.g. http://127.0.0.1:8090
	HTTP    *http.Client
}

// NewClient builds an engine client with a 15s default timeout.
func NewClient(baseURL string) *Client {
	return &Client{BaseURL: strings.TrimSuffix(baseURL, "/"), HTTP: &http.Client{Timeout: 15 * time.Second}}
}

// StreamURL builds the upstream media URL for an engine file ID.
func (c *Client) StreamURL(hash string, engineFileID int) string {
	return fmt.Sprintf("%s/stream?link=%s&index=%d&play", c.BaseURL, url.QueryEscape(hash), engineFileID)
}

// Add adds a torrent by link (magnet, raw hash, or .torrent URL).
func (c *Client) Add(ctx context.Context, link string) (TorrentStatus, error) {
	var st TorrentStatus
	if err := c.post(ctx, "/torrents", map[string]string{"action": "add", "link": link}, &st); err != nil {
		return TorrentStatus{}, err
	}
	return st, nil
}

// Get returns the torrent status, or ErrNotFound when dropped/unknown.
func (c *Client) Get(ctx context.Context, hash string) (TorrentStatus, error) {
	var st TorrentStatus
	if err := c.post(ctx, "/torrents", map[string]string{"action": "get", "hash": hash}, &st); err != nil {
		return TorrentStatus{}, err
	}
	return st, nil
}

// Cache returns the cache state, or ErrNotFound when dropped/unknown.
func (c *Client) Cache(ctx context.Context, hash string) (CacheState, error) {
	var cs CacheState
	if err := c.post(ctx, "/cache", map[string]string{"action": "get", "hash": hash}, &cs); err != nil {
		return CacheState{}, err
	}
	return cs, nil
}

type engineError struct {
	Error string `json:"error"`
}

func (c *Client) post(ctx context.Context, path string, body map[string]string, out any) error {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "crispy-companion/1")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode >= 400 {
		var ee engineError
		msg := http.StatusText(resp.StatusCode)
		if json.Unmarshal(data, &ee) == nil && ee.Error != "" {
			msg = ee.Error
		}
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return fmt.Errorf("engine %s: %s", path, msg)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode engine %s: %w", path, err)
	}
	return nil
}

// FileByIndex maps our 0-based fileIdx onto the engine's 1-based,
// path-sorted file ID (engineID = fileIdx+1 after bounds check).
// Ordering divergence vs Stremio's archive-order fileIdx is PLAN Q2: for
// multi-file torrents the wrong file may address-match; the stats response
// carries the engine file list so a future webapp can reconcile.
func FileByIndex(st TorrentStatus, fileIdx int) (FileStat, error) {
	if fileIdx < 0 || fileIdx >= len(st.FileStats) {
		return FileStat{}, fmt.Errorf("file index %d out of range (%d files)", fileIdx, len(st.FileStats))
	}
	return st.FileStats[fileIdx], nil
}

// ProgressForFile computes the buffered fraction of one engine file from the
// cache piece map. File byte offset is derived from the path-sorted list
// (engine IDs are position+1); piece overlap is counted byte-precise.
func ProgressForFile(st TorrentStatus, cs CacheState, file FileStat) float64 {
	if file.Length <= 0 || cs.PiecesLength <= 0 {
		return 0
	}
	var offset int64
	for _, f := range st.FileStats {
		if f.ID < file.ID {
			offset += f.Length
		}
	}
	end := offset + file.Length
	pieceLen := cs.PiecesLength
	first := offset / pieceLen
	last := (end - 1) / pieceLen
	var done int64
	for p := first; p <= last; p++ {
		ps, ok := cs.Pieces[strconv.FormatInt(p, 10)]
		if !ok || !ps.Completed {
			continue
		}
		lo := max(p*pieceLen, offset)
		hi := min((p+1)*pieceLen, end)
		if hi > lo {
			done += hi - lo
		}
	}
	progress := float64(done) / float64(file.Length)
	if progress < 0 {
		return 0
	}
	if progress > 1 {
		return 1
	}
	return progress
}

// DownloadedForFile is the buffered byte count inside one engine file.
func DownloadedForFile(st TorrentStatus, cs CacheState, file FileStat) int64 {
	return int64(ProgressForFile(st, cs, file) * float64(file.Length))
}
