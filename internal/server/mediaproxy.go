package server

import (
	"io"
	"net"
	"net/http"
	"time"
)

// mediaHeaderAllowlist is the set of upstream response headers relayed to
// clients. Everything else (engine internals, hop-by-hop headers) is dropped.
var mediaHeaderAllowlist = []string{
	"Content-Type",
	"Content-Length",
	"Content-Range",
	"Accept-Ranges",
	"ETag",
	"Last-Modified",
}

// mediaClient relays media bytes (engine streams and /v1/proxy targets) and
// deliberately has NO overall timeout — playback streams can run for hours.
// Only per-phase timeouts are set, so a stalled upstream still fails fast.
// (The engine control client's 15s total timeout is for short control calls;
// using it for media truncated long streams.)
var mediaClient = &http.Client{
	Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   16,
		DisableCompression:    true,
	},
}

// upstreamRequest builds the upstream GET/HEAD request for a media URL,
// forwarding Range and If-Range so seek and resume requests pass through.
func upstreamRequest(r *http.Request, url string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, url, nil)
	if err != nil {
		return nil, err
	}
	if v := r.Header.Get("Range"); v != "" {
		req.Header.Set("Range", v)
	}
	if v := r.Header.Get("If-Range"); v != "" {
		req.Header.Set("If-Range", v)
	}
	req.Header.Set("User-Agent", "crispy-companion/1")
	return req, nil
}

// relayMedia streams an upstream media response to the client: allowlisted
// headers only, status verbatim (200/206/416/...), body copied unless HEAD.
// No buffering, no transformation; client aborts propagate upstream via the
// request context, which cancels the body copy.
func relayMedia(w http.ResponseWriter, r *http.Request, up *http.Response) {
	defer up.Body.Close()
	for _, h := range mediaHeaderAllowlist {
		if v := up.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.WriteHeader(up.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, up.Body)
}

// drainAndClose discards a bounded amount of an unrelayed upstream body so
// the connection can be reused, then closes it.
func drainAndClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 32<<10))
	_ = resp.Body.Close()
}
