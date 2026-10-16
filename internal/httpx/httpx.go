// Package httpx implements the companion's HTTP perimeter middleware.
//
// PLAN.md §6 controls, enforced in this order per request:
//  1. Host allowlist (DNS-rebinding defense)
//  2. Sec-Fetch-Site gate (cross-site allowed only with allowlisted Origin)
//  3. Exact Origin allowlist + CORS reflection (never "*")
//  4. Private Network Access (PNA) response headers
//
// Authentication (bearer tokens) lives in internal/pairing; this package
// only decides whether the request's *provenance* may proceed.
package httpx

import (
	"net"
	"net/http"
	"strconv"
	"strings"
)

// Policy is the effective perimeter policy, derived from config at startup.
type Policy struct {
	// AllowedHosts is the exact Host allowlist, e.g. "127.0.0.1:11470".
	AllowedHosts map[string]struct{}
	// AllowedOrigins is the exact Origin allowlist, e.g. "https://crispy.app".
	AllowedOrigins map[string]struct{}
	// DevLoopbackOrigins additionally allows http://127.0.0.1:<port> and
	// http://localhost:<port> origins when CRISPY_DEV=true.
	DevLoopbackOrigins bool
}

// NewPolicy builds a Policy from explicit host/origin lists.
func NewPolicy(hosts, origins []string, devLoopback bool) Policy {
	p := Policy{
		AllowedHosts:       make(map[string]struct{}, len(hosts)),
		AllowedOrigins:     make(map[string]struct{}, len(origins)),
		DevLoopbackOrigins: devLoopback,
	}
	for _, h := range hosts {
		p.AllowedHosts[strings.ToLower(h)] = struct{}{}
	}
	for _, o := range origins {
		p.AllowedOrigins[o] = struct{}{}
	}
	return p
}

// HostAllowed reports whether r.Host is exactly allowlisted.
func (p Policy) HostAllowed(host string) bool {
	_, ok := p.AllowedHosts[strings.ToLower(host)]
	return ok
}

// OriginAllowed reports whether an Origin value is allowlisted.
// Empty origin ("none", e.g. <video> tags, curl) is NOT allowed by this
// function — callers decide per-endpoint whether absent-origin is acceptable.
func (p Policy) OriginAllowed(origin string) bool {
	if _, ok := p.AllowedOrigins[origin]; ok {
		return true
	}
	if p.DevLoopbackOrigins && isLoopbackOrigin(origin) {
		return true
	}
	return false
}

func isLoopbackOrigin(origin string) bool {
	if !strings.HasPrefix(origin, "http://127.0.0.1:") && !strings.HasPrefix(origin, "http://localhost:") {
		return false
	}
	rest := origin[len("http://"):]
	if i := strings.LastIndex(rest, ":"); i >= 0 {
		if port, err := strconv.Atoi(rest[i+1:]); err == nil && port >= 1 && port <= 65535 {
			host := rest[:i]
			return host == "127.0.0.1" || host == "localhost"
		}
	}
	return false
}

// IsLoopbackAddr reports whether addr is a loopback IP (for /healthz scoping).
func IsLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	if ip != nil {
		return ip.IsLoopback()
	}
	return host == "localhost"
}

// Middleware enforces Host + Sec-Fetch-Site + CORS on every request.
//
// Public, unauthenticated endpoints (/companion/ping, /healthz) still pass
// through here: provenance is checked, authentication is not.
func (p Policy) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 1. Host allowlist — DNS rebinding defense (PLAN §6 T2).
		if !p.HostAllowed(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}

		origin := r.Header.Get("Origin")
		fetchSite := r.Header.Get("Sec-Fetch-Site")

		// 2. Sec-Fetch-Site gate. Real browsers label HTTPS→loopback and
		// HTTPS→tailnet fetches as cross-site, so cross-site is allowed
		// when — and only when — the request carries an allowlisted
		// Origin; the Origin check below remains the actual gate
		// (PLAN §6 T1/T2, M5 shared mode).
		if strings.EqualFold(fetchSite, "cross-site") {
			if origin == "" || !p.OriginAllowed(origin) {
				http.Error(w, "cross-site request rejected", http.StatusForbidden)
				return
			}
		} else {
			switch strings.ToLower(fetchSite) {
			case "same-origin", "same-site", "none", "":
				// allowed to proceed to Origin check
			default:
				http.Error(w, "unrecognized Sec-Fetch-Site", http.StatusForbidden)
				return
			}
		}

		// Preflight handling.
		if r.Method == http.MethodOptions {
			p.servePreflight(w, r, origin)
			return
		}

		// 3. CORS for actual requests with an Origin header.
		if origin != "" {
			if !p.OriginAllowed(origin) {
				http.Error(w, "origin not allowed", http.StatusForbidden)
				return
			}
			setCORS(w, origin)
		}
		// Absent Origin (no-cors <video>, curl, same-origin navigations):
		// allowed through here; per-endpoint auth + Referer checks decide.

		next.ServeHTTP(w, r)
	})
}

func (p Policy) servePreflight(w http.ResponseWriter, r *http.Request, origin string) {
	if origin == "" || !p.OriginAllowed(origin) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	setCORS(w, origin)
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, HEAD, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Crispy-Client")
	w.Header().Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
}

func setCORS(w http.ResponseWriter, origin string) {
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Vary", "Origin")
	w.Header().Set("Access-Control-Allow-Credentials", "false")
	// Private Network Access (PLAN §10): advertise opt-in for HTTPS→loopback.
	w.Header().Set("Access-Control-Allow-Private-Network", "true")
}
