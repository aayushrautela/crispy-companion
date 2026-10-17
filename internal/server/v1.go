package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crispy-streaming/crispy-companion/internal/engine"
)

// BufferConfig is the readiness policy for /v1 (spec 1.1). Readiness is
// time-based, not percent-based: a torrent is ready when at least MinBytes
// of the selected file is contiguous from its start AND that runway covers
// StartSeconds of playback at the observed bitrate. The bitrate is estimated
// from contiguous-byte growth over BitrateWindowSeconds (0 until measurable,
// which skips the seconds clause). Sustainable is advisory only: it reports
// whether the current download speed keeps up with the estimated bitrate
// (within SustainableMarginPercent).
type BufferConfig struct {
	MinBytes                 int64
	StartSeconds             float64
	BitrateWindowSeconds     float64
	SustainableMarginPercent float64
}

func (c BufferConfig) withDefaults() BufferConfig {
	if c.MinBytes <= 0 {
		c.MinBytes = 4 << 20 // 4 MiB
	}
	if c.StartSeconds <= 0 {
		c.StartSeconds = 10
	}
	if c.BitrateWindowSeconds <= 0 {
		c.BitrateWindowSeconds = 10
	}
	if c.SustainableMarginPercent <= 0 {
		c.SustainableMarginPercent = 100
	}
	return c
}

// computeReady applies the readiness rule from spec 1.1.
func computeReady(cfg BufferConfig, complete bool, contiguous int64, bitrate float64) bool {
	if complete {
		return true
	}
	if contiguous < cfg.MinBytes {
		return false
	}
	if bitrate <= 0 {
		return true // unmeasurable bitrate: bytes clause alone decides
	}
	return float64(contiguous)/bitrate >= cfg.StartSeconds
}

func sustainableFor(cfg BufferConfig, complete bool, bitrate, downloadSpeed float64) bool {
	if complete {
		return true
	}
	if bitrate <= 0 {
		return downloadSpeed > 0
	}
	return downloadSpeed*100 >= bitrate*cfg.SustainableMarginPercent
}

type bitrateSample struct {
	at    time.Time
	bytes int64
}

// V1State is the process-wide /v1 bookkeeping: pending file selections, the
// create rate limiter, bitrate samples, and the guarded proxy client. It is
// shared across listeners (loopback + shared) because Deps is copied.
type V1State struct {
	cfg      BufferConfig
	mu       sync.Mutex
	selected map[string]int
	creates  map[string][]time.Time
	samples  map[string][]bitrateSample

	proxyClient *http.Client
}

// NewV1State builds the /v1 state. allowInternal relaxes the /v1/proxy target
// guard so tests can reach loopback httptest servers; production passes false.
func NewV1State(cfg BufferConfig, allowInternal bool) *V1State {
	cfg = cfg.withDefaults()
	return &V1State{
		cfg:      cfg,
		selected: make(map[string]int),
		creates:  make(map[string][]time.Time),
		samples:  make(map[string][]bitrateSample),
		proxyClient: &http.Client{
			Transport: &http.Transport{
				DialContext:           proxyDialContext(allowInternal),
				ResponseHeaderTimeout: 30 * time.Second,
				IdleConnTimeout:       90 * time.Second,
				MaxIdleConns:          16,
				MaxIdleConnsPerHost:   8,
				DisableCompression:    true,
			},
			CheckRedirect: proxyRedirectPolicy,
		},
	}
}

const v1CreatesPerMinute = 10

// allowCreate is a per-token sliding-window limiter for POST /v1/torrents.
func (s *V1State) allowCreate(token string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := now.Add(-time.Minute)
	kept := s.creates[token][:0]
	for _, t := range s.creates[token] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= v1CreatesPerMinute {
		s.creates[token] = kept
		return false
	}
	s.creates[token] = append(kept, now)
	return true
}

// observeBitrate records a contiguous-bytes sample and returns the observed
// coverage rate in bytes/sec: (newest-oldest) bytes over the oldest sample
// still inside the window. Needs at least two samples spanning 1s; otherwise
// the estimate is 0 (unmeasurable, per spec).
func (s *V1State) observeBitrate(id string, contiguous int64, now time.Time) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	window := time.Duration(s.cfg.BitrateWindowSeconds * float64(time.Second))
	if window <= 0 {
		window = 10 * time.Second
	}
	samples := s.samples[id]
	if n := len(samples); n > 0 && now.Sub(samples[n-1].at) < 250*time.Millisecond && samples[n-1].bytes == contiguous {
		samples[n-1] = bitrateSample{at: now, bytes: contiguous}
	} else {
		samples = append(samples, bitrateSample{at: now, bytes: contiguous})
	}
	cutoff := now.Add(-window)
	kept := samples[:0]
	for _, sm := range samples {
		if sm.at.After(cutoff) {
			kept = append(kept, sm)
		}
	}
	s.samples[id] = kept

	if len(kept) < 2 {
		return 0
	}
	span := kept[len(kept)-1].at.Sub(kept[0].at)
	if span < time.Second {
		return 0
	}
	delta := kept[len(kept)-1].bytes - kept[0].bytes
	if delta <= 0 {
		return 0
	}
	return float64(delta) / span.Seconds()
}

func (s *V1State) setSelected(id string, idx int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.selected[id] = idx
}

func (s *V1State) selectedFor(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if idx, ok := s.selected[id]; ok {
		return idx
	}
	return -1
}

func (s *V1State) forget(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.selected, id)
	delete(s.samples, id)
}

// --- wire types -----------------------------------------------------------

type v1ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type v1ErrorEnvelope struct {
	Error v1ErrorBody `json:"error"`
}

type v1File struct {
	Idx       int    `json:"idx"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	VideoLike bool   `json:"videoLike"`
	Wanted    bool   `json:"wanted"`
}

type v1Buffer struct {
	ContiguousBytes int64   `json:"contiguousBytes"`
	Seconds         float64 `json:"seconds"`
	Bitrate         int64   `json:"bitrate"`
	Sustainable     bool    `json:"sustainable"`
}

type v1Peers struct {
	Connected int `json:"connected"`
	Seeding   int `json:"seeding"`
	Leeching  int `json:"leeching"`
}

type v1Torrent struct {
	ID               string       `json:"id"`
	Name             *string      `json:"name"`
	State            string       `json:"state"`
	Ready            bool         `json:"ready"`
	Buffer           v1Buffer     `json:"buffer"`
	Progress         float64      `json:"progress"`
	MetadataProgress float64      `json:"metadataProgress"`
	Downloaded       int64        `json:"downloaded"`
	Size             int64        `json:"size"`
	AmountLeft       int64        `json:"amountLeft"`
	DownloadSpeed    int64        `json:"downloadSpeed"`
	UploadSpeed      int64        `json:"uploadSpeed"`
	ETA              *int64       `json:"eta"`
	Peers            v1Peers      `json:"peers"`
	FileIdx          int          `json:"fileIdx"`
	SelectionReason  string       `json:"selectionReason"`
	Files            []v1File     `json:"files"`
	Error            *v1ErrorBody `json:"error"`
}

type v1TorrentResponse struct {
	v1Torrent
	RetryAfterMs int `json:"retryAfterMs"`
}

type v1CreateResponse struct {
	ID    string `json:"id"`
	State string `json:"state"`
	Ready bool   `json:"ready"`
}

// engineUnavailableMsg is what users see when the companion's own engine is
// not answering. The underlying client error is a Go transport string
// ("Post http://127.0.0.1:8090/torrents: dial tcp ...: connection refused")
// that nobody can act on; the reason (usually a missing engine binary) is
// logged by the companion at startup instead.
const engineUnavailableMsg = "the companion's torrent engine is not responding — check the companion app on the paired machine"

func writeV1Error(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, v1ErrorEnvelope{Error: v1ErrorBody{Code: code, Message: message}})
}

// --- selection ------------------------------------------------------------

var videoExts = map[string]struct{}{
	".mp4": {}, ".m4v": {}, ".mkv": {}, ".webm": {}, ".avi": {}, ".mov": {},
	".ts": {}, ".m2ts": {}, ".flv": {}, ".wmv": {}, ".mpg": {}, ".mpeg": {},
	".ogv": {}, ".ogm": {}, ".divx": {}, ".vob": {},
}

func videoLikeName(p string) bool {
	_, ok := videoExts[strings.ToLower(path.Ext(p))]
	return ok
}

// v1SelectIndex resolves the wanted file: a client-requested index is honored
// verbatim (out of range is an error, never a silent guess); otherwise the
// largest video-like file wins (the old guessFileIdx trap - spec 6).
func v1SelectIndex(files []engine.FileStat, requested int) (int, string, *v1ErrorBody) {
	if requested >= 0 {
		if requested < len(files) {
			return requested, "requested", nil
		}
		return -1, "requested", &v1ErrorBody{
			Code:    "bad-file-index",
			Message: fmt.Sprintf("requested fileIdx %d is out of range (%d files)", requested, len(files)),
		}
	}
	best := -1
	var bestSize int64
	for i, f := range files {
		if !videoLikeName(f.Path) {
			continue
		}
		if best == -1 || f.Length > bestSize {
			best, bestSize = i, f.Length
		}
	}
	if best == -1 {
		return -1, "auto-largest", &v1ErrorBody{
			Code:    "no-video-file",
			Message: "no video-like file found in torrent",
		}
	}
	return best, "auto-largest", nil
}

// --- status body ----------------------------------------------------------

// v1TorrentBody assembles the spec 1.1 torrent object from an engine status.
func (d Deps) v1TorrentBody(ctx context.Context, id string, st engine.TorrentStatus, requested int) v1Torrent {
	out := v1Torrent{
		ID:              strings.ToLower(st.Hash),
		FileIdx:         -1,
		Files:           make([]v1File, 0, len(st.FileStats)),
		DownloadSpeed:   int64(math.Round(st.DownloadSpeed)),
		UploadSpeed:     int64(math.Round(st.UploadSpeed)),
		SelectionReason: "auto-largest",
	}
	if out.ID == "" {
		out.ID = id
	}
	if st.Name != "" && !strings.HasPrefix(st.Name, "infohash:") {
		name := st.Name
		out.Name = &name
	}
	out.Peers = v1Peers{
		Connected: st.ActivePeers,
		Seeding:   st.ConnectedSeeders,
		Leeching:  max(0, st.ActivePeers-st.ConnectedSeeders),
	}
	for i, f := range st.FileStats {
		out.Files = append(out.Files, v1File{
			Idx:       i,
			Name:      mediaName(f.Path),
			Path:      f.Path,
			Size:      f.Length,
			VideoLike: videoLikeName(f.Path),
		})
	}
	if len(st.FileStats) > 0 {
		out.MetadataProgress = 1
	}

	sel, reason, selErr := v1SelectIndex(st.FileStats, requested)
	out.SelectionReason = reason
	var complete bool
	if sel >= 0 {
		out.FileIdx = sel
		out.Files[sel].Wanted = true
		file := st.FileStats[sel]
		out.Size = file.Length
		if d.Torrents != nil {
			if cs, err := d.Torrents.Cache(ctx, out.ID); err == nil {
				out.Progress = engine.ProgressForFile(st, cs, file)
				out.Downloaded = engine.DownloadedForFile(st, cs, file)
				contiguous := engine.ContiguousForFile(st, cs, file)
				bitrate := d.V1.observeBitrate(out.ID, contiguous, time.Now())
				complete = out.Progress >= 1
				out.AmountLeft = max(0, file.Length-out.Downloaded)
				out.Buffer = v1Buffer{ContiguousBytes: contiguous}
				if bitrate > 0 {
					out.Buffer.Bitrate = int64(math.Round(bitrate))
					out.Buffer.Seconds = math.Round(float64(contiguous)/bitrate*10) / 10
				}
				out.Ready = computeReady(d.V1.cfg, complete, contiguous, bitrate)
				out.Buffer.Sustainable = sustainableFor(d.V1.cfg, complete, bitrate, st.DownloadSpeed)
				if !complete && st.DownloadSpeed > 0 && out.AmountLeft > 0 {
					eta := int64(math.Round(float64(out.AmountLeft) / st.DownloadSpeed))
					out.ETA = &eta
				}
			}
		}
	}

	switch {
	case st.Stat == engine.StatClosed || st.Stat == engine.StatInDB:
		out.State = "error"
		out.Error = &v1ErrorBody{Code: "engine-lost", Message: "torrent is not loaded on the engine"}
	case len(st.FileStats) == 0 || st.Stat == engine.StatAdded || st.Stat == engine.StatGettingInfo:
		out.State = "metadata"
	case selErr != nil:
		out.State = "error"
		out.Error = selErr
	case st.Stat == engine.StatPreload:
		out.State = "downloading"
	case st.Stat == engine.StatWorking:
		switch {
		case complete:
			out.State = "seeding"
		case st.DownloadSpeed > 0:
			out.State = "downloading"
		default:
			out.State = "stalled"
		}
	default:
		out.State = "metadata"
	}
	return out
}

// v1InitialState is the best-effort state for the 202 create response.
func v1InitialState(st engine.TorrentStatus) string {
	if len(st.FileStats) > 0 && (st.Stat == engine.StatWorking || st.Stat == engine.StatPreload) {
		return "downloading"
	}
	return "metadata"
}

// --- helpers --------------------------------------------------------------

// v1Link normalizes a client-supplied torrent reference into the engine's
// accepted link form: raw hashes (hex or base32) become tracker-enriched
// magnets; magnets and http(s) .torrent URLs pass sanitizeLink.
func v1Link(raw string) string {
	if isInfoHash(raw) {
		if h, err := normalizeHash(raw); err == nil {
			return engine.MagnetForHash(h)
		}
	}
	return sanitizeLink(raw)
}

// v1Get fetches engine status, transparently re-adding a torrent the engine
// dropped (idle timeout) when a link is known. Unknown ids come back as
// engine.ErrNotFound; unrecoverable closed/in-db statuses come back as-is.
func (d Deps) v1Get(ctx context.Context, hash string) (engine.TorrentStatus, error) {
	st, err := d.Torrents.Get(ctx, hash)
	if err != nil && !errors.Is(err, engine.ErrNotFound) {
		return st, err
	}
	unloaded := errors.Is(err, engine.ErrNotFound) || st.Stat == engine.StatClosed || st.Stat == engine.StatInDB
	if !unloaded {
		return st, nil
	}
	link, ok := d.Links.Get(hash)
	if !ok {
		if err == nil {
			err = engine.ErrNotFound
		}
		return st, err
	}
	if _, err := d.Torrents.Add(ctx, link); err != nil {
		return st, err
	}
	return d.Torrents.Get(ctx, hash)
}

// v1Auth enforces the bearer token (or ?token= on media GETs only) and
// writes the 401 envelope on failure.
func (d Deps) v1Auth(w http.ResponseWriter, r *http.Request, allowQuery bool) (string, bool) {
	tok := bearerOf(r)
	if tok == "" && allowQuery {
		tok = r.URL.Query().Get("token")
	}
	if tok == "" {
		writeV1Error(w, http.StatusUnauthorized, "unauthorized", "missing companion token")
		return "", false
	}
	if d.Tokens == nil {
		writeV1Error(w, http.StatusUnauthorized, "unauthorized", "unknown companion token")
		return "", false
	}
	if _, ok := d.Tokens.Lookup(tok); !ok {
		writeV1Error(w, http.StatusUnauthorized, "unauthorized", "unknown companion token")
		return "", false
	}
	return tok, true
}

func (d Deps) v1PathHash(w http.ResponseWriter, r *http.Request) (string, bool) {
	hash, err := normalizeHash(r.PathValue("id"))
	if err != nil {
		writeV1Error(w, http.StatusNotFound, "not-found", "unknown torrent id")
		return "", false
	}
	return hash, true
}

func clientHeaderMissing(r *http.Request) bool {
	return r.Header.Get("X-Crispy-Client") == ""
}

// --- handlers -------------------------------------------------------------

// handleV1Create implements POST /v1/torrents (spec 1.1).
func (d Deps) handleV1Create(w http.ResponseWriter, r *http.Request) {
	token, ok := d.v1Auth(w, r, false)
	if !ok {
		return
	}
	if d.Torrents == nil {
		writeV1Error(w, http.StatusServiceUnavailable, "engine-unavailable", "no engine configured")
		return
	}
	if clientHeaderMissing(r) {
		writeV1Error(w, http.StatusBadRequest, "bad-request", "missing X-Crispy-Client")
		return
	}
	if !d.V1.allowCreate(token, time.Now()) {
		writeV1Error(w, http.StatusTooManyRequests, "rate-limited", "too many torrent creations; try again in a minute")
		return
	}
	var body struct {
		URL     string `json:"url"`
		Link    string `json:"link"`
		FileIdx *int   `json:"fileIdx"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeV1Error(w, http.StatusBadRequest, "bad-request", "invalid JSON body")
		return
	}
	if body.FileIdx != nil && *body.FileIdx < 0 {
		writeV1Error(w, http.StatusBadRequest, "bad-request", "fileIdx must be >= 0")
		return
	}
	raw := body.URL
	if raw == "" {
		raw = body.Link
	}
	link := v1Link(raw)
	if link == "" {
		writeV1Error(w, http.StatusBadRequest, "bad-request", "url must be a magnet, an infohash, or an http(s) .torrent URL")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	st, err := d.Torrents.Add(ctx, link)
	if err != nil {
		writeV1Error(w, http.StatusConflict, "engine-unavailable", engineUnavailableMsg)
		return
	}
	id := strings.ToLower(st.Hash)
	if id == "" && isInfoHash(raw) {
		if h, nerr := normalizeHash(raw); nerr == nil {
			id = h
		}
	}
	if id == "" {
		writeV1Error(w, http.StatusConflict, "engine-unavailable", "engine did not return a torrent id")
		return
	}
	d.Links.Set(id, link)
	sel := -1
	if body.FileIdx != nil {
		sel = *body.FileIdx
	}
	d.V1.setSelected(id, sel)
	writeJSON(w, http.StatusAccepted, v1CreateResponse{ID: id, State: v1InitialState(st), Ready: false})
}

// handleV1Status implements GET /v1/torrents/{id} (spec 1.1).
func (d Deps) handleV1Status(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.v1Auth(w, r, false); !ok {
		return
	}
	hash, ok := d.v1PathHash(w, r)
	if !ok {
		return
	}
	if d.Torrents == nil {
		writeV1Error(w, http.StatusServiceUnavailable, "engine-unavailable", "no engine configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	st, err := d.v1Get(ctx, hash)
	if errors.Is(err, engine.ErrNotFound) {
		writeV1Error(w, http.StatusNotFound, "not-found", "unknown or expired torrent id")
		return
	}
	if err != nil {
		writeV1Error(w, http.StatusServiceUnavailable, "engine-unavailable", engineUnavailableMsg)
		return
	}
	body := d.v1TorrentBody(ctx, hash, st, d.V1.selectedFor(hash))
	writeJSON(w, http.StatusOK, v1TorrentResponse{v1Torrent: body, RetryAfterMs: 500})
}

// handleV1List implements GET /v1/torrents (spec 1.1).
func (d Deps) handleV1List(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.v1Auth(w, r, false); !ok {
		return
	}
	if d.Torrents == nil {
		writeV1Error(w, http.StatusServiceUnavailable, "engine-unavailable", "no engine configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	list, err := d.Torrents.List(ctx)
	if err != nil {
		writeV1Error(w, http.StatusServiceUnavailable, "engine-unavailable", engineUnavailableMsg)
		return
	}
	torrents := make([]v1Torrent, 0, len(list))
	for _, st := range list {
		if st.Hash == "" {
			continue
		}
		id := strings.ToLower(st.Hash)
		torrents = append(torrents, d.v1TorrentBody(ctx, id, st, d.V1.selectedFor(id)))
	}
	writeJSON(w, http.StatusOK, struct {
		Torrents []v1Torrent `json:"torrents"`
	}{Torrents: torrents})
}

// handleV1Delete implements DELETE /v1/torrents/{id} (spec 1.1). The engine
// `rem` action stops the torrent, forgets it, and frees its (in-memory) piece
// cache, so ?purgeCache needs no extra engine call; the parameter is accepted
// for API compatibility.
func (d Deps) handleV1Delete(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.v1Auth(w, r, false); !ok {
		return
	}
	if clientHeaderMissing(r) {
		writeV1Error(w, http.StatusBadRequest, "bad-request", "missing X-Crispy-Client")
		return
	}
	hash, ok := d.v1PathHash(w, r)
	if !ok {
		return
	}
	if d.Torrents == nil {
		writeV1Error(w, http.StatusServiceUnavailable, "engine-unavailable", "no engine configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := d.Torrents.Rem(ctx, hash); err != nil {
		writeV1Error(w, http.StatusServiceUnavailable, "engine-unavailable", engineUnavailableMsg)
		return
	}
	d.Links.Delete(hash)
	d.V1.forget(hash)
	w.WriteHeader(http.StatusNoContent)
}

// handleV1Stream implements GET|HEAD /v1/torrents/{id}/stream (spec 1.2):
// proxied engine bytes with Range/If-Range/HEAD passthrough. A 404 tells the
// client the engine no longer has the torrent (re-POST once, then resume).
func (d Deps) handleV1Stream(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.v1Auth(w, r, true); !ok {
		return
	}
	hash, ok := d.v1PathHash(w, r)
	if !ok {
		return
	}
	if d.Torrents == nil {
		writeV1Error(w, http.StatusServiceUnavailable, "engine-unavailable", "no engine configured")
		return
	}
	fileIdx := d.V1.selectedFor(hash)
	if fq := r.URL.Query().Get("file"); fq != "" {
		n, err := strconv.Atoi(fq)
		if err != nil || n < 0 {
			writeV1Error(w, http.StatusBadRequest, "bad-request", "file must be a non-negative index")
			return
		}
		fileIdx = n
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	st, err := d.v1Get(ctx, hash)
	if errors.Is(err, engine.ErrNotFound) || (err == nil && (st.Stat == engine.StatClosed || st.Stat == engine.StatInDB)) {
		writeV1Error(w, http.StatusNotFound, "not-found", "torrent is not on the engine; create it again")
		return
	}
	if err != nil {
		writeV1Error(w, http.StatusServiceUnavailable, "engine-unavailable", engineUnavailableMsg)
		return
	}
	if fileIdx < 0 {
		sel, _, selErr := v1SelectIndex(st.FileStats, -1)
		if selErr != nil {
			writeV1Error(w, http.StatusNotFound, "not-found", selErr.Message)
			return
		}
		fileIdx = sel
	}
	file, err := engine.FileByIndex(st, fileIdx)
	if err != nil {
		if len(st.FileStats) == 0 {
			writeV1Error(w, http.StatusNotFound, "not-found", "torrent metadata is not ready")
		} else {
			writeV1Error(w, http.StatusNotFound, "not-found", "file index out of range")
		}
		return
	}
	select {
	case d.MediaSem <- struct{}{}:
		defer func() { <-d.MediaSem }()
	default:
		writeV1Error(w, http.StatusServiceUnavailable, "engine-unavailable", "too many concurrent streams")
		return
	}
	upReq, err := upstreamRequest(r, d.Torrents.StreamURL(hash, file.ID))
	if err != nil {
		writeV1Error(w, http.StatusBadRequest, "bad-request", "invalid stream request")
		return
	}
	upResp, err := mediaClient.Do(upReq)
	if err != nil {
		writeV1Error(w, http.StatusBadGateway, "bad-gateway", "engine stream failed")
		return
	}
	switch {
	case upResp.StatusCode == http.StatusNotFound:
		drainAndClose(upResp)
		writeV1Error(w, http.StatusNotFound, "not-found", "torrent dropped from the engine; create it again")
	case upResp.StatusCode >= 500:
		drainAndClose(upResp)
		writeV1Error(w, http.StatusServiceUnavailable, "engine-unavailable", "engine stream failed")
	default:
		relayMedia(w, r, upResp)
	}
}

// --- /v1/proxy ------------------------------------------------------------

var (
	errTargetBlocked = errors.New("target address not allowed")
	errCrossScheme   = errors.New("cross-scheme redirect")
)

// checkPublicIP rejects targets that are not global-unicast addresses:
// loopback, RFC1918, link-local, CGNAT, unspecified, multicast and reserved.
func checkPublicIP(ip net.IP) error {
	if ip == nil {
		return errTargetBlocked
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return errTargetBlocked
	}
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 0 || (v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127) || v4[0] >= 240 {
			return errTargetBlocked
		}
	}
	return nil
}

// validateProxyTarget is the pre-flight guard for /v1/proxy: literal IPs are
// checked directly, hostnames are resolved and every returned address must be
// global-unicast.
func validateProxyTarget(ctx context.Context, u *url.URL) error {
	host := u.Hostname()
	if host == "" {
		return errTargetBlocked
	}
	if ip := net.ParseIP(host); ip != nil {
		return checkPublicIP(ip)
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return err
	}
	if len(ips) == 0 {
		return errTargetBlocked
	}
	for _, ip := range ips {
		if err := checkPublicIP(ip.IP); err != nil {
			return err
		}
	}
	return nil
}

// proxyDialContext resolves the target at dial time and only connects to
// validated addresses, so DNS answers that change between validation and
// connection cannot smuggle in a private address.
func proxyDialContext(allowInternal bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		lastErr := errTargetBlocked
		for _, ip := range ips {
			if !allowInternal {
				if berr := checkPublicIP(ip.IP); berr != nil {
					lastErr = berr
					continue
				}
			}
			conn, derr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
			if derr == nil {
				return conn, nil
			}
			lastErr = derr
		}
		return nil, lastErr
	}
}

// proxyRedirectPolicy rejects cross-scheme redirects and re-limits the hop
// count; every hop still passes through the guarded dialer.
func proxyRedirectPolicy(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("too many redirects")
	}
	if req.URL.Scheme != via[0].URL.Scheme {
		return fmt.Errorf("%w: %s to %s", errCrossScheme, via[0].URL.Scheme, req.URL.Scheme)
	}
	return nil
}

// handleV1Proxy implements GET|HEAD /v1/proxy (spec 1.3): stream an absolute
// http(s) URL through the companion with Range passthrough and the guarded
// dialer. 5xx from the target is propagated verbatim.
func (d Deps) handleV1Proxy(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.v1Auth(w, r, true); !ok {
		return
	}
	target := r.URL.Query().Get("target")
	if target == "" {
		writeV1Error(w, http.StatusBadRequest, "bad-request", "target query parameter is required")
		return
	}
	u, err := url.Parse(target)
	if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		writeV1Error(w, http.StatusBadRequest, "bad-request", "target must be an absolute http(s) URL")
		return
	}
	if !d.ProxyAllowInternal {
		vctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		verr := validateProxyTarget(vctx, u)
		cancel()
		if verr != nil {
			if errors.Is(verr, errTargetBlocked) {
				writeV1Error(w, http.StatusForbidden, "forbidden", "target address is not allowed")
			} else {
				writeV1Error(w, http.StatusBadGateway, "bad-gateway", "target lookup failed")
			}
			return
		}
	}
	upReq, err := upstreamRequest(r, u.String())
	if err != nil {
		writeV1Error(w, http.StatusBadRequest, "bad-request", "invalid target")
		return
	}
	upResp, err := d.V1.proxyClient.Do(upReq)
	if err != nil {
		switch {
		case errors.Is(err, errTargetBlocked):
			writeV1Error(w, http.StatusForbidden, "forbidden", "target address is not allowed")
		case errors.Is(err, errCrossScheme):
			writeV1Error(w, http.StatusForbidden, "forbidden", "cross-scheme redirect refused")
		default:
			writeV1Error(w, http.StatusBadGateway, "bad-gateway", "target request failed")
		}
		return
	}
	relayMedia(w, r, upResp)
}
