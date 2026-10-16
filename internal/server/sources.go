package server

import (
	"encoding/base32"
	"fmt"
	"strings"
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

// mediaName is the display name: last path segment of the engine file path.
func mediaName(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}
