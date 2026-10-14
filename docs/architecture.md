# Architecture (M1)

Port of PLAN.md §4–§5 figures. The shim is the only exposed surface.

```
browser (https, canonical origin)
  │ probe → GET /companion/ping            (public, CORS-reflected)
  │ pair  → POST /companion/pair           (M2: RFC 8628 via crispy-server)
  │ api   → POST /{ih}/create              (M3; M1: 401/503)
  │         GET  /{ih}/{fi}/stats.json     (M3; M1: 401/503)
  │ media → GET  /{ih}/{fi}?token=…        (M3 proxy; M1: 401/503)
  ▼
shim :11470 (loopback only)
  middleware: Host allowlist → Sec-Fetch-Site gate → exact-Origin CORS + PNA
  state-changing routes additionally require X-Crispy-Client (preflight-forcing)
  auth: constant-time Bearer compare (header wins; ?token= on media only)
  │
  ├── engine 127.0.0.1:8090 (TorrServer child, M3 mapping)
  └── crispy-server (HTTPS, M2 device grant; fail-closed when unreachable)
```

Packages: `internal/config` (TOML subset + env), `internal/httpx`
(perimeter), `internal/pairing` (0600 token store + single-pending
window), `internal/engine` (loopback-only supervisor), `internal/server`
(route table), `internal/notify` (M1 log stub; portal/toast in M4).
