# Crispy Companion — Plan

**Status:** `proposal / pre-code` · **Last updated:** 2026-10-14  
**Type:** RFC + implementation roadmap — authoritative starting point for the `crispy-companion` repo. Read first, then decide if we code.  
**Outside the Crispy tree?** Yes — new repo `crispy-companion`. Crispy (webapp) will be updated later to speak this contract. Until then Crispy's `streamingServer.ts` is the interface spec, not work in flight.

> **Scope in one line:** a headless Go daemon that fixes CORS for `<video>` playback and provides safe on-device torrent streaming via an embedded TorrServer engine. Logged-in users only. Windowless — pairing is approved in the logged-in webapp via crispy-server's device grant and confirmed with a desktop notification. Linux/Flatpak first, Windows & macOS next, Android deferred.

---

## Table of contents

1. [Why this exists](#1-why-this-exists)
2. [Prior art & key findings (compressed)](#2-prior-art--key-findings-compressed)
3. [Goals / non-goals](#3-goals--non-goals)
4. [Architecture](#4-architecture)
5. [Protocol contract](#5-protocol-contract)
6. [Security model](#6-security-model)
7. [Pairing & identity in detail](#7-pairing--identity-in-detail)
8. [Operating modes](#8-operating-modes)
9. [Configuration & on-disk layout](#9-configuration--on-disk-layout)
10. [The CORS fix (player on HTTPS ↔ loopback shim)](#10-the-cors-fix-player-on-https--loopback-shim)
11. [TorrServer integration](#11-torrserver-integration)
12. [Platform packaging](#12-platform-packaging)
13. [Repo shape, deps & tooling](#13-repo-shape-deps--tooling)
14. [Crispy-side integration points (later, separate repo)](#14-crispy-side-integration-points-later-separate-repo)
15. [Milestones + acceptance](#15-milestones--acceptance)
16. [Testing & verification matrix](#16-testing--verification-matrix)
17. [Distribution, licenses & updates](#17-distribution-licenses--updates)
18. [Risks & mitigations](#18-risks--mitigations)
19. [Decisions log](#19-decisions-log)
20. [Open questions](#20-open-questions)

---

## 1. Why this exists

Crispy's worker (today at `src/worker/handlers/streamingServer.ts`, contract at `ENGINE_URL = http://127.0.0.1:11470`, Stremio streaming-server shape `POST /{infoHash}/create`, `GET /{infoHash}/{fileIdx}/stats.json`, media at `/{infoHash}/{fileIdx}`) currently has no first-party local engine — users install closed-source Stremio Service to fill it. Stremio's engine:

- runs CORS `*` and **no auth at all** (official read: `Access-Control-Allow-Origin: *` for all origins on `127.0.0.1:11470`, no token anywhere on the HTTP surface, `NO_CORS=1` env even disables those checks; HTTPS on 12470 via `stremio.rocks` wildcards exists only for mixed-content). Any website can detect it, start torrents under the user's IP, and stream fetched bytes.

Requirements driving this project: **fix CORS without involving the torrent engine in browser-facing traffic; make the torrent engine safe against foul actors; serve streams over loopback by default and over the local network/Internet optionally, without friction.**

---

## 2. Prior art & key findings (compressed)

- **TorrServer** (`YouROK/TorrServer`, Go, GPL-3.0, actively maintained, ~3.1k★): `8080`/`8090` default; usable flags `--ip` (repeatable bind), `--httpauth` (Basic from `accs.db` JSON `{user:pass}`), full HTTPS matrix (`--ssl`/`--sslport`/`--sslcert`/`--sslkey`/`--force-https`/`--http-media`/`--https-only`, hot-reload + self-signed default on localhost+hostname+LAN IPs; `--https-only` never opens HTTP), `--rdb`/`--maxsize`/`--path`, socks5h `--proxyurl`/`--proxymode` (tracker/peers/full), **WAF** (Settings→WAF / `/waf` API, hot, CIDR+referer) over-riding prior `wip.txt`/`bip.txt`, PeerGuardian-style `blocklist` file, DuckDNS+`acme.sh` DNS-01 docs for trusted LAN certs. Docker `TS_*` mirror and `-gst` HLS builds exist. Releases as `TorrServer-<platform>-<arch>`.
- **`crispy`** worker contract: probe chain is `http(s) → urlResolver → magnet (magnet-uri) → infoHash → POST /{ih}/create (5 s, tolerates failure) → poll stats every 500 ms up to 60 s until `streamProgress >= 0.05 → media URL `${ENGINE_URL}/{ih}/{fileIdx}`. `FileStats.streamProgress/streamLen/streamName/downloaded/pieceLength`. Keys `torrent:${ih}:${fileIdx}`.
- **Flatpak**: `--share=network` enough to serve on the LAN, ports `>1024` free; the lockdown is filesystem. Flathub prefers Go-vendored builds; Background portal handles autostart. Can't open host firewalls.
- **Further findings worth keeping:**
  - KDE Connect / LocalSend / TOFU tier precedents and a short list of pair-model CVEs — client-asserted device names are spoofable; approval popups are raceable (see `CVE-2020-26164`-class "pairing-window hijack"); short static codes are brute-gateable (`CVE-2025-32898`); unauth discovery = identity spoof (`CVE-2025-32900`). Backend-verified identity and a single-pending, expiring, ID-bound pairing gate are the antidote. **Pairing UX bug lessons included verbatim in the chat compression `docs/` lineage instead of this file.**
  - `RFC 8252` loopback hygiene: bind the IP literal, `SO_REUSEADDR/REUSEPORT` off on Linux path (Windows `SO_EXCLUSIVEADDRUSE`).
  - Tailscale `tsnet`: an embedded-tailnode Go library whose `ListenTLS` + `WhoIs` solves LAN certs without firewall pokes. Kept as M5 headline for shared mode.

Stremio details are in the extended design brief (provenance: `stremio-runtime` reverse-eng README, `Stremio/server-docker` README, `stremio-server-go` package docs) — this plan distills only what the contract needs.

---

## 3. Goals / non-goals

### Goals (v1-shipped)

| G | Description |
|---|---|
| G1 | Fix CORS/mixed-content for Crispy webapp playback (HTTPS page ↔ loopback shim) |
| G2 | Provide torrent playback through a locally-cached BitTorrent engine (`TorrServer`) |
| G3 | **Windowless** companion — no persistent settings GUI; only a desktop notification and a pairing token file |
| G4 | Logged-in users only (pairing approved from a logged-in Crispy account via crispy-server's RFC 8628 device grant; unauthenticated `GET`s are never elevated) |
| G5 | Safe by default on loopback; explicit approval for first use, token-required thereafter |
| G6 | Opt-in shared access over the LAN/Internet, **identity-verified and wire-secure** |
| G7 | Single Go module, cross-buildable for Flatpak / Windows / macOS; Android deferred |
| G8 | Clear Crispy-side integration contract so `crispy` updates can land later without coupling the two repos |

### Non-goals (v1)

- Torznab/indexer search (pure streamer; relies on Crispy to supply magnets/URLs)
- Windowed settings/GUI (`TorrServer`'s own web UI on `127.0.0.1:8090` is the management surface)
- iOS, UPnP/NAT-PMP, DNLA/Fuse/WebDAV bots, self-signed "accept before use" heuristics
- Analytics/telemetry in the companion

---

## 4. Architecture

```
                ┌──────────────────── authenticated, hardened ────────────────────┐
     browser ──►│   Crispy webapp (https, canonical origin)                     │
     (HTTPS)   │   + AuthContext/Supabase session                               │
                └──────────────┬────────────────────────────────────────────────┘
                               │ probe → ping
                               │ pair → /pair (RFC 8628 via crispy-server)
                               │ API → /{infoHash}/create, …/stats.json  (token header)
                               │ media → /{infoHash}/{fileIdx}?token=…       (query token)
                               ▼
                ┌──────────────────────────────────────┐
                │  crispy-companion — Go shim            │  XDG data dir
                │  :11470 loopback (only exposed surface) │  tokens, config, logs
                │  ┌─────────────────────────────────┐  │  engine flags,
                │  │ middleware: CORS/Host/Sec-Fetch │  │  WAF templating
                │  │ rate-limits, single-pendingPair │  │
                │  │ device-grant poller → web server │  │
                │  │ notifications (OS portal)        │  │
                │  └──┬───────────────┬──────────────┘  │
                │     │               │                  │
                │     │ proxy+stats   │ optional M5      │
                │     ▼               ▼                  │
                │  torrserver ───►  tsnet node           │
                │  127.0.0.1:8090    tailAddr:443 (TLS)  │
                └──────────────────────────────────────┘
```

Invariants:

- **TorrServer never leaves `127.0.0.1:8090`.** Only the shim is ever reachable off-box.
- No cookies — bearer token header (API) and `?token=` (media). Media needs the query form because `<video>` can't set headers.
- WAF/`--httpauth` on the engine remain **defense-in-depth** (shim injects them there), never the perimeter.
- `SO_REUSEPORT=0`/`SO_REUSEADDR=0` on the shim listener (Linux; Windows `SO_EXCLUSIVEADDRUSE`). Random port is not used — we displace Stremio at `:11470` intentionally (Stremio-server-go precedent), but we fail loudly if rebounding would shadow us.

---

## 5. Protocol contract

Version `protocol = 1` (advertised in `/companion/ping`). `companion/` endpoints are added; Stremio-compatible streaming paths at root are preserved so existing Crispy work needs minimal diffs.

### 5.1 Endpoints

| Route | Purpose | Auth | CORS |
|---|---|---|---|
| `GET /companion/ping` | Probe/discovery — `{name:"crispy-companion",version,protocol,requiresLogin:true,paired:bool}` (last field UX-only, low sensitivity) | none | CORS on, public info only |
| `POST /companion/pair` | Start pairing (RFC 8628 bootstrap via crispy-server). Header `X-Crispy-Client`. Returns `{pendingPairId, verificationUrl}`. Contains no identity secrets either way. | Origin + Host + Sec-Fetch validated; rate-limited | CORS: canonical origins only |
| `GET /companion/status` | `{paired, mode, engineOk, pairingState:"idle"|"pending", pendingFor?}` | token | CORS: canonical origins only |
| `POST /companion/revoke` | Revoke own token / sign out companion from this browser | token | same |
| `POST /{infoHash}/create` | Stremio-compatible (payload left Stremio-compatible; body `{token?}` accepted but header wins). Triggers engine add. | token (header) | same |
| `GET /{infoHash}/{fileIdx}/stats.json` | Buffer/progress probe (polled ~500 ms by webapp) — `{streamProgress:0..1, streamLen, streamName, downloaded, pieceLength}` | token (header) | same |
| `GET /{infoHash}/{fileIdx}` (`HEAD` also) | **Proxied media** onto TorrServer `GET /stream?link=&index=&play` (`Range`/`If-Range` preserved, `Accept-Ranges: bytes`, `206` passthrough, no buffering, abort propagation). | token (`?token=` or `Authorization`, header wins) | CORS: canonical origins only for `fetch`; `?token=` accepted for `<video>` (no Origin) with `Referer` allowlist where present |
| `GET /healthz` | Loopback liveness | none (127.0.0.1-only + no data) | local fetches |

- `{infoHash}`: strict `40-hex` or `32-base32` (narrowed before any parsing/use). `{fileIdx}`: unsigned decimal (string → `uint`, bounds-checked).
- Body limit: 32/64 KiB on `create`, exact. JSON/query forms only; nothing shell-ish.

### 5.2 Pairing sub-protocol

Pairing rides crispy-server's **existing RFC 8628 Device Authorization Grant** (`crispy-server/src/modules/auth/device-authorization.service.ts` — the flow already powering TV login). No Supabase session token ever crosses the origin → local-process boundary.

```
webapp (logged in)      companion (127.0.0.1:11470)      crispy-server (HTTPS)
       │ POST /companion/pair            │
       │────────────►│ POST /v1/auth/device/authorize    │
       │             │ {clientId:"crispy-companion",     │  (public endpoint,
       │             │  deviceName:"DESKTOP-AB12"}       │   no credentials)
       │             │◄── userCode, deviceCode,          │
       │             │    verificationUriComplete        │
       │◄─200 {status:"pending", verificationUrl,        │
       │       pendingPairId}            │               │
       │ renders in-app approval card:   │               │
       │ "Approve Crispy Companion on    │  POST /v1/auth/device/verification
       │  DESKTOP-AB12? Signed in as     │  POST /v1/auth/device/verification/approve
       │  alice@example.com [Approve]"   │──────────────►│  (user Bearer — server knows
       │                                 │               │   the account; verified identity)
       │ (polled) GET /companion/status  │               │
       │◄─{pairingState:"pending"}       │ POST /v1/auth/device/token (poll per interval)
       │                                 │◄─ status:"approved" + cp_pat_ device token + user
       │                                 │ 1. stores cp_pat_ + deviceId (server session)
       │                                 │ 2. mints LOCAL companion API token (≥128-bit)
       │                                 │ 3. tokens.json: localToken → {userId,email,deviceId,pairedAt}
       │                                 │ 4. desktop notification: "Crispy Companion
       │                                 │    paired for alice@example.com [Revoke]"
       │◄─ status poll: {paired:true, token:<localCompanionToken>, user:{...}}
       │ stores local token → API calls auth-header, <video> ?token=
```

Rules (invariants unchanged): one pending pairing per companion; server-side TTL ≈15 min for codes, ≈90 s exposed pair-init window; 409 while pending; 429 on init rate (≤5/min/IP); 410 after expiry. The **approval gate lives in the logged-in webapp** — the server authenticates the approver, so the displayed identity is verified by construction; this is strictly stronger than forwarding any token to the local process. The desktop notification becomes a **confirmation with a Revoke action** — not the gate — which also removes OS notification-button plumbing from the pairing critical path. Third-party pages get nothing: `verificationUrl` never leaves CORS/Host/Sec-Fetch-locked responses (T1), and approval itself requires a server-authenticated session.

Revocation: server-side `DELETE /v1/auth/devices/{deviceId}` revokes the device **and** its `cp_pat_` in one transaction (already implemented). The companion lazily re-validates its `cp_pat_` against the server; a 401 on any device-scoped call ⇒ wipe local tokens, notify, and Crispy shows a re-pair prompt. The Crispy settings "connected devices" UI (`GET /v1/auth/devices`) covers companions for free.

Server-side change required (small): add `"crispy-companion"` to `DEVICE_AUTHORIZATION_CLIENT_IDS` (currently `['crispy-tv']` in `device-authorization.service.ts`) and persist `deviceType: 'companion'` on the device row. **No new endpoints.**

### 5.3 TorrServer mapping (what ships in M3, spikes in M0)

- `POST /{ih}/create` → internally `POST http://127.0.0.1:8090/torrents` with `{action:"add", link:"magnet:?xt=urn:btih:{ih}&..."}`; on hash-only create synthesize link `magnet:?xt=urn:btih:{ih}`. Poll `{action:"get", hash}` until `stat_string` settled and file list populated.
- `GET /{ih}/{fi}/stats.json` → derived from `/torrents` file list + `/cache/state?hash=<hash>` (pieces/cache) to compute contiguous-into-file buffered fraction as `streamProgress` (0-1). Keep the **exact** 5 % → "ready" gate from the webapp unchanged in v1; refine later.
- `GET /{ih}/{fi}` → proxied `GET http://127.0.0.1:8090/stream?link=&index=<mapped>&play` (index mapped via ordered, path-sorted file list; keep config knob for ordering if `torrentFiles` ordering ever proves to differ).

> **M0 spike task:** confirm `CacheState → streamProgress` mapping against `server.js` semantics and verify `<video>` `Range` passthrough latency (206).

---

## 6. Security model

### 6.1 Threat model

| # | Attacker | Capability | Goal | In/out of scope |
|---|---|---|---|---|
| T1 | Malicious webpage on `evil.com` | Can run JS in any pinned tab | Start torrents under user's IP, read stats/progress, stream contents, detect local server | **IN** |
| T2 | DNS-rebinding page | Can point `evil.com → 127.0.0.1` | Same | **IN** |
| T3 | Local malware / `curl` | Unrestricted on `user` account | Abuse engine, steal token file | OUT (user account = privileged; token file read == privileged — Jupyter's stance; not pretended otherwise) |
| T4 | Rogue LAN device (neighbor, roommate's TV) | Can reach `addr:port` on LAN | Use engine as open proxy | **IN** |
| T5 | On-path LAN observer | Can sniff LAN segments | Steal streams/tokens | **IN** only on raw-LAN mode (warned); **IN** on tsnet mode is eliminated |
| T6 | Malicious torrent peers | Can send crafted pieces | Corrupt playback / DoS cache | **IN** (peer blocklists, TorrServer's own sanity) |
| T7 | Lookalike account on Crispy | Can register `alice+2@example` | Impersonate approval | **IN** (email in notification) |

### 6.2 Controls (per-threat layering, no novel crypto)

- **Structural separation (§4):** engine bound to loopback. Shim is the only surface.
- **Loopback is not a trust boundary.** Browsers enforce nothing loopback-native. One race notes: same-`user` processes share the token file.
- **Exact Origin allowlist** (canonical `crispy` web origins + small localhost port set, no regexes, no wildcards) — `Access-Control-Allow-Origin` **reflected** to the request's origin on allow, `Vary: Origin`, `Access-Control-Allow-Credentials: false`. No `*`.
- **Exact Host allowlist** (`127.0.0.1:11470` / `localhost:11470`) — DNS rebinding → Host `evil.com:11470` rejected even if Origin slipped.
- **`Sec-Fetch-Site` gate** — reject `cross-site`; allow `same-origin`/`none`. Close Firefox/IE-era form-path gaps alongside (3).
- **Preflight-forced mutations** — every state-changing route requires the `X-Crispy-Client` header and/or JSON body so an attacker's simple-GET/POST trick never reaches handler logic.
- **PNA response** — `Access-Control-Allow-Private-Network: true` on allowed origins (mixed-content stack's core).
- **Token, constant-time** — per-pairing URL-safe base64 `≥128b` via `crypto/rand`; `subtle.ConstantTimeCompare` on check. Header `Authorization: Bearer …` for API; `?token=` only accepted on media (de-scoped blast: one endpoint).
- **Listener hygiene** — fixed port `:11470` displacing Stremio is intentional (Stremio-server-go precedent); add `SO_REUSEADDR/REUSEPORT` off and loud failure if shadowing would occur.
- **Pairing-window discipline** — one pending per instance, `pendingTTL` 60–120 s, rate-limit (`≈5/min/IP` on handshake, `429`), notification action carries `pendingId`, approve does not tag `CORS: *` onto token issuance.
- **Identity via server-side device grant** (crispy-server RFC 8628; §7) — the companion never receives nor verifies a bearer session token. The server mints a device-scoped `cp_pat_` only after logged-in approval; the companion separately mints its local token. No JWT secrets in the artefact.
- **Log redaction** — never log raw bearer tokens (local companion tokens, `cp_pat_`), or full query media tokens. Redact in any pii-limited dump.
- **No ambient auth** — no cookies anywhere.

What is explicitly **not** promised: protection from a compromised local user account, from injected XSS on the canonical web origins (which would already steal session tokens), or from peer-supplied byte-level corruption beyond the engine's own piece verification.

---

## 7. Pairing & identity in detail

- **Identity broker: crispy-server** (the full backend we own), via its already-implemented RFC 8628 device grant. The companion is treated exactly like a TV: an untrusted, inputless device that a logged-in browser approves. Supabase remains the session issuer *inside* the canonical webapp + server trust boundary; it never reaches the companion.
- **What crosses the loopback boundary:** only a short-lived, context-free *pairing intent* (the verification URL) from companion → browser, and a locally-minted companion token from companion → browser **after** server-confirmed approval. No Supabase access/refresh tokens and no `cp_pat_` ever transit the browser↔companion channel (the `cp_pat_` travels TLS-direct server → companion).
- **Stored state:** `tokens.json` (0600) — `localCompanionToken → {userId, email, deviceId, pairedAt, lastSeenAt}`; `device-session.json` (0600) — `cp_pat_` + `deviceId` for server calls (identity re-assertion, revocation checks). Revoke = delete local token; server revoke additionally invalidates the `cp_pat_`, which the companion detects and honours.
- **Enforcement policies:**
  - New pairing requires online reachability of crispy-server (**fail-closed**, D8); existing local tokens keep working offline.
  - Logged-out browsers cannot pair — approval endpoints require the user Bearer; the webapp shows "Sign in to enable companion".
  - Self-hosters re-point `crispyServer.url`; the same grant must exist server-side (it does, modulo the clientId allowlist).
- **Why not forward Supabase access tokens** (earlier draft mechanism, superseded): forwarding a live session credential to *any* local process swaps a contained web-session credential for a listener-identity problem (anything squatting `127.0.0.1:11470` first could harvest it), widens the token's exposure surface, and yields no server-side revocation or device inventory. The device grant inverts all three: the **server** mints a narrow device token only after user approval, and revocation is one `DELETE` away. A "verify a caller-supplied token via Supabase `/auth/v1/user`" adapter remains possible internally for exotic self-hosts but is **not** the production path.

---

## 8. Operating modes

| Mode | When | What the user observes | Security notes |
|---|---|---|---|
| **`loopback` (default)** | Install Flatpak / run binary → launch → open Crispy | One notification to approve your logged-in account → then transparent. Next launches: token re-used. | TOFU-like frictionlessness without silent TOFU — explicit one-time approval, permanent token thereafter |
| **`shared: tsnet` (M5 recommended)** | Opt-in flag `shared.mode = tsnet` | Second device (phone/TV browser on `https://crispy.app`) calls same handshake → notification on desktop: "`alice@…` wants to pair from Pixel — Approve?" | WireGuard + auto Let's Encrypt via `ListenTLS`; `WhoIs` identity available as second verification leg; **no firewalls** poked |
| **`shared: raw` (fallback warning)** | `shared.mode = raw` + explicit LAN `shared.listenAddr` | Same Approve-as-above | Unencrypted HTTP across LAN segment; token in LAN URL → warned, not closed-lipped; LAN `shared` never auto-promotes, requires explicit opt-in |
| `pairing: strict` flag | `pairing.mode = strict` (future knob) | Always approval; never silent | Planned, not v1 default |

- `shared` is never implied by `tsnet` anywhere else in flow; kernel never bridges the two (the shim needing to keep `engine` on loopback invariant).

---

## 9. Configuration & on-disk layout

### 9.1 Files (Flatpak example path, platforms map 1:1)

```
$XDG_DATA_HOME/crispy-companion/          # Flatpak: ~/.var/app/io.crispy.Companion/data
  config.toml                             # per §9.2
  tokens.json                             # 0600, companion tokens→user mapping
  pairing-state.json                      # ephemeral pendingId + expiry
  logs/companion.log                      # ring buffer, redacted
  cache/torrserver/                       # delegated (configured), defaults to same parent
```

- macOS/Windows: `$XDG_DATA_HOME` is `~/Library/Application Support/…` / `%LOCALAPPDATA%\Crispy Companion`; keys `0600`-equivalent (Windows ACL limited to current user SID).
- Linux non-Flatpak / dev: `~/.config/crispy-companion` + `~/.local/share/crispy-companion` (honour `XDG_*`).

### 9.2 `config.toml` (annotated defaults)

```toml
[listen]
addr = "127.0.0.1"
port = 11470

[engine]
addr = "127.0.0.1"
port = 8090
autoStart = true              # shim spawns/monitors torrserver child
binaryPath = ""               # override pinned bundle
maxCacheBytes = 0             # 0 = TorrServer default (--maxsize behaviour), configurable per-GUI

[auth]
# Canonical origins to reflect (no regex, no *). Self-hosters override.
allowedOrigins = ["https://crispy.app", "https://app.crispy.example"]  # + localhost dev entries when CRISPY_DEV=true
requireOnlineVerification = true
tokenFile = "tokens.json"

pairing.pendingTTLSeconds = 90
pairing.singlePending = true  # always true; not knob
pairing.maxAttemptsPerMinute = 5

[crispyServer]
url = "${CANONICAL_SERVER_URL}"        # e.g. https://api.crispy.example
clientId = "crispy-companion"          # must be allowlisted server-side (RFC 8628)

[shared]
mode = "off"                  # off | tsnet | raw
listenAddr = ""               # raw only: explicit LAN IP, never 0.0.0.0 by default
maxConcurrentMediaProxies = 32

[logging]
level = "info"                # debug|info|warn|error — debug emits request IDs, never tokens
ringBytes = 262144
```

Env overrides mirror leaves (e.g., `CRISPY_COMPANION_LISTEN_PORT`).

---

## 10. The CORS fix (player on HTTPS ↔ loopback shim)

- Webapp served over `https://crispy.app` fetches `http://127.0.0.1:11470/…`. Two orthogonal problems: CORS and mixed-content/PNA.
- Shim responses on allowed origins set: `Access-Control-Allow-Origin: <request origin>`, `Access-Control-Allow-Methods: GET,POST,HEAD,OPTIONS`, `Access-Control-Allow-Headers: Authorization, Content-Type, X-Crispy-Client`, `Access-Control-Allow-Private-Network: true`, `Vary: Origin` + `Access-Control-Request-Private-Network` preflight handling.
- `<video>` playback does **not** require CORS (and `FetchMode=no-cors`), but its cross-origin is allowed via `?token=`; for browsers that send `Referer: https://crispy.app` (origin-only) the allowlist is re-checked.
- Stremio's fallback (`Access-Control-Allow-Origin: *`) is explicitly rejected for production builds.

---

## 11. TorrServer integration

- **Binary bundle:** ship version-pinned `TorrServer-<platform>-<arch>` artefacts per `goreleaser`/manifest, SHA-pinned per arch; FOSS MatriX line recommended; `-gst` line deferred.
- **Runtime flags** shim generates: `--ip 127.0.0.1`, `--port 8090`, `--path <cacheDir>` plus internal-only `--rdb`/`--maxsize` as configured, `--proxyurl`/`--proxymode` if user configured (sign a separate plan if needed), and internal `--httpauth`/`--webdav`/DLNA switches **off** where resolvable — the engine is intentionally minimal.
- **WAF:** loopback-bound engines have it off; shim owns the surface. Keep peer `blocklist` enabled.
- **Outside host firewalls:** only `tsnet`'s wire leaves the machine. Doc note: warn firewalls only in raw-LAN section, do not relax auto.
- **If self-hosting enables TLS elsewhere (reverse-proxies / LAN certs):** keep `tsnet` TLS on tsnet tailaddr and shim's loopback host unchanged; do not mix wire-path TLS via DuckDNS `acme.sh` docs that are recorded in the design lineage — tsnet remains the recommended path here. Recorded broader TorrServer TLS knobs remain reference, not v1 plans.
- **Update story:** companion ships the engine version; a check job pings `YouROK/TorrServer` releases (`MatriX.*`) weekly and offers to upgrade after displaying the changelog (F-Droid builds treated as ref). Downgrade path is to last pinned binary.

---

## 12. Platform packaging

### 12.1 Shared

- Language/runtime: **Go** (`≥ 1.23`), stdlib-first; only small, reviewable dep set per-platform (notifications proxy, optional `tsnet`).
- Listener hygiene §6 is shared across builds.

### 12.2 Linux (Flatpak) — primary

- Manifest: `io.crispy.Companion`, permission `--share=network`, FS locked to `xdg-data/crispy-companion` (no `home:`), `org.freedesktop.portal.Background` (autostart), `org.freedesktop.portal.Notification` (actions).
- Notifications via portal's `org.freedesktop.Notifications` `AddNotification` / `Notify` with buttons — not `libnotify` alone, so Flatpak-sandboxed.
- engine binary as `files/bin/torrserver`.

### 12.3 Windows

- Single `crispy-companion.exe` + companion updater stub; installer target is `crispy-companion-{arch}.zip` now, `MSI` later. Autostart via `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` (user, not `HKLM`). Toasts via `windows-toast-notifications` family (buttons required; class allows self-hosted upgrade path via `goreleaser/winget`).
- Notarization/signature: `signtool` path reserved; verified builds note when unsigned dev path is used.

### 12.4 macOS

- `crispy-companion` binary + `LaunchAgent` `io.crispy.Companion.plist` (`KeepAlive` true on macOS bundle installs; user-stoppable). Notarization `notarytool` reserved. Notifications via `UNUserNotificationCenter` bridge (cgo `objc` shim small; `osascript` fallback without buttons documented).
- Universal arm64+x86_64 slice (Goreleaser `lipo` slice).

### 12.5 Android — deferred

- Precedent `ru.yourok.torrserve` (F-Droid, `MatriX.141.FOSS` line, `arm64-v8a … x86_64`) is engine-only. For parity we produce via `gomobile bind` → `.aar` + thin Kotlin foreground service (required for any long-lived loopback server; battery nuance: workers, wakelocks noted). `6.27`-class Stremio/Syncthing `M0Rf…` wrappers on `gomobile` semantics are already proven. Ship detail is in extended notes (§2 / `M6` below) and called out as **deferred — not v1**.

---

## 13. Repo shape, deps & tooling

```
.
├─ PLAN.md                    # ← this file
├─ cmd/crispy-companion/      # shim entry point
├─ internal/
│  ├─ config/
│  ├─ engine/                 # torrserver lifecycle + mapping
│  ├─ httpx/                  # CORS/Host/Sec-Fetch, auth, rate-limits
│  ├─ pairing/                # device-grant poll + pairing window + tokens
│  ├─ notify/                 # per-OS portal/toast impls
│  ├─ tsnode/                 # tsnet integration (opt-in)
│  └─ proxy/                  # media passthrough + stats derivation
├─ packaging/
│  ├─ flatpak/
│  ├─ windows/
│  └─ macos/
├─ docs/architecture.md      # port of this plan's §4-5 figures
├─ go.mod                     # pinned minimal deps
├─ .golangci.yml
├─ Makefile / goreleaser.yml
└─ LICENSE
```

Deps (candidate, pinned, no brokers):

- `tailscale.com/tsnet` — optional tag `tsnet`.
- notification backends: `gen2brain/beeep` (base), platform action extensions small.
- No deps for CORS/auth/limiter (stdlib).

---

## 14. Crispy-side integration points (later, separate repo)

> "Server will be updated later to support this local companion" — **these are real work items, spelled out here as interface requirements, but executed in the `crispy` repo**. File them as `crispy:companion-support` (logged-in-only) issues.

1. **Probe & prefs:** `GET http://127.0.0.1:11470/companion/ping` on page load (1–2 s timeout). Settings → Advanced: companion URL (default `127.0.0.1:11470`) + status dot (grey/green/pending) + "Pair companion" button.
2. **Pairing wiring:** "Pair companion" → `POST /companion/pair` → render verification card (reuse the TV device-approval pattern; server endpoints `POST /v1/auth/device/verification` and `…/approve`) showing device name + signed-in account → on Approve, the companion's server-side poll resolves → `GET /companion/status` flips to `{paired:true, token:…}` → store the **companion local token** in `localStorage` scoped to origin. A confirmation desktop notification lands on the machine (Revoke action available); it is informational, not the gate.
3. **Worker edits (§5 contract):**
   ```ts
   // src/worker/handlers/streamingServer.ts: add
   // - ENGINE_URL = configurable (default http://127.0.0.1:11470) → ping-aware.
   // - attach: every POST /{ih}/create and GET …/stats.json includes
   //   header Authorization: Bearer <companionPairToken> + X-Crispy-Client: crispy-web/<ver>.
   // - media URLs returned as `${ENGINE_URL}/{infoHash}/{fileIdx}?token=${pairToken}`.
   // - Origin is canonical (nothing to spoof) — don't override it.
   // Keep magnet-parse → infoHash logic; keep polling/sanctioned 5 % gate.
   ```
4. **Auth guard:** companion `paired:false` state → Crispy shows "Sign in to enable companion" — fallback to none (or future Stremio fallback prefs behind a kill-switch, not documented).
5. **LAN/shared UX:** second device radios for "scan QR / enter code" wait until `M5` ships; document step 1 shipping as loopback-only first.
6. **Error surfacing:** 401/403/429 typed from worker up to UI toast; sibling to `HttpError` with action suggestion (revoke/re-pair text).

---

## 15. Milestones + acceptance

| Milestone | Scope | Acceptance |
|---|---|---|
| **M0 — Spikes (throwaway, branch-local)** | Drive TorrServer locally (`/torrents` `action=add/get`, `/stream?link=&index=&play` range, `/cache/state`) against multiple `Range:` headers; deliver `<video>` PASSTHROUGH latency note. Mixed-content/PNA matrix: Chrome/Firefox/Safari over canonical HTTPS origin → `http://127.0.0.1:11470` with/without `Allow-Private-Network`. Flatpak portal notification-actions probe on sample app | `docs/spikes/M0-notes.md` written; `streamProgress → ready` mapping documented; PNA-required headers enumerated |
| **M1 — Skeleton (builds & registers)** | Go project init, config loader (§9), logging (redacted), listeners hygiene §6, engine lifecycle (spawn/monitor torrserver), `GET /companion/ping` + `GET /healthz`, CORS/Host/Sec-Fetch middleware on allowed origins | `go test ./…` + `make {lint,vet,build}` green; `curl -v` probe matrix matches §10 preflights; mixed-content player stability on canonical HTTPS origin |
| **M2 — Pairing + auth** | Device-grant poller (§5.2/§7; crispy-server `crispy-companion` clientId), single-pending state machine, confirmation notifications per-OS (no approvals via notification), persistent `tokens.json` + `device-session.json` stores, auth middleware (header wins), `POST /companion/pair` + `GET /companion/status` + `POST /companion/revoke`, rate-limits, 401/403/409/410/429 typed | Logged-in Crispy browser initiates pair → approves in the signed-in in-app card (`alice@…`) → companion receives approval via crispy-server poll → subsequent calls succeed with the local token. Pairing URL never leaves CORS-locked origin (T1 proof). Unpaired `POST /…/create` → 401. Server-revoked device → 401 on next re-validation |
| **M3 — Streaming contract** | Stremio-compatible create/stats (5 % gate honoured, field mapping from engine), media proxy (`Range`/`If-Range`/`HEAD`→`206`), fan-out limits, log redaction on media keys | Existing Crispy torrent UX plays against `:11470` as it played against Stremio (seek + resume + head-request parity) on canonical origin; torrent from magnet supplied by Crispy metadata resolves inside companion against real tracker/peers |
| **M4 — Packaging (primary: Flatpak; within—zip for Win/mac drafts)** | Flatpak manifest + portal notification/BG; Windows zip + HKCU autostart; macOS `LaunchAgent` draft | `flatpak build && flatpak run io.crispy.Companion` loopback is default; system notification action pair works on Flatpak/Linux first; win/mac "pair via file-approved fallback `crispy-companion pair show/approve`" path functional |
| **M5 — Shared mode** | `tsnet` (opt-in `shared.mode=tsnet`) `ListenTLS` auto-certs + second surface on `tailAddr:443`; raw-LAN `listenAddr` explicit-warning | Phone/tablet/TV browser on canonical origin pairs and plays via tsnet; raw LAN with warning text; `WHOIS` logged as second leg where tsnet is used |
| **M6 — Android (deferred, not v1)** | `gomobile bind` → `.aar` + Kotlin foreground service; gallery of `ru.yourok.torrserve` F-Droid engine-only path as interop doc | Marked deferred — ticket shaped, not shipped |
| **M7 — Hardening & release** | Threat checklist rerun (§6), CORS matrix full-browser pass (Chrome/Firefox/Safari/Edge + mobile), Host/Pending-window fuzz of pairing, rate-limit soak, update mechanism wired (`MatriX.*` tracker), docs polish, governance/licensing note | Pen-test style report (§16 matrix) in `docs/security-review.md`; `goreleaser` producing tagged, checksummed artefacts; Flathub line filed once Flatpak build lands |

---

## 16. Testing & verification matrix

- **CORS matrix (actual, browser-driven):** canonical origins × (`GET`/`POST` +custom header) × (`no-cors` `<video>` GET) against `FetchMode=cors|samesite=no-cors` in Chrome/Firefox/Safari/Edge × `Origin: allowed | disallowed | absent` × rebind Host override → must return pass/fail as §10 dictates. Automated via `internal/httpx_test.go` + a tiny e2e harness serving a real `https://*.test` origin (or canonical domain draft).
- **Pairing gates:** rate-limit trip, single-pending FIFO, server TTL/expiry 410, identity assertion via server (approval by `alice@…` yields an alice-marked device + confirmation; a different approver yields a different, honestly-shown identity), logged-out 401, crispy-server unreachable → fail-closed for new pairings, revoked device → 401 on next re-validation.
- **Proxy:** `Range:` passthrough ↔ TorrServer `stream` (0-0, open-ended, out-of-bounds), `HEAD`, abort-by-close, `If-Range`, `206`/`200` semantics.
- **Flatpak sandbox:** FS-leak checks (no `home:` readable), port binding at `:11470` owned by shim, engine at `:8090` never externally reachable, portal notification actions promotion.
- **Windows/macOS smoke** (installed): autostart toggle, kill + restart, notification click-through.
- See also `docs/security-review.md` for a tabular version of the same matrix.

---

## 17. Distribution, licenses & updates

- **License:** `GPL-3.0` for this repo — mandatory because bundled `TorrServer` binaries are `GPL-3.0` (copyleft passes through distribution). Add `DCO` + `REUSE` headers on additions. Checked-in engine binaries only via pinned, hashed fetch; source mirrors listed in attestations.
- **Releases:** tagged `crispy-companion-vX.Y.Z` via `goreleaser` (checksums + SBOM). Engine version pinned as `torrserver-vMatriX.*`.
- **Channels:** GitHub releases (zip), Flathub (`io.crispy.Companion`), Windows Store/winget and Homebrew once signing/notarization lands.
- **Updates:** companion self-check weekly on `YouROK/TorrServer` + companion releases; engine change log shown before offered restart. Roll back to last-pinned version is a restart.
- **Privacy:** no companion telemetry. Local logs are `info`-limited, rotated, and redacted; diagnostics bundles are manual export only.

---

## 18. Risks & mitigations

| Risk | Impact | Mitigant |
|---|---|---|
| Canonical web origins drift (canonical deployment vs self-hosted forks) | CORS allowlist diverges | `allowedOrigins` config override (with config-override shape, explicit deny on `*`), validated against. Ship defaults match canonical deployment. |
| Desktop notification with actions unshipable early on platform | Pairing ritual blocked | Fallback: `crispy-companion pair show|approve <pendingId>` CLI + token file print; notification action is the head, CLI path is the tail. Both share `pendingId`. |
| crispy-server grant drift (clientId allowlist, envelope shapes) | Pairing path breaks | Versioned server contract (`protocol=1` fields), CI contract test against anon calls (`401`/`authorization_pending` expected, not 500), server-side change landed before companion M2 acceptance |
| TorrServer API / binary shape drift | Mapping breaks | Version-pinned bundle + `engine_tests` soak; `M0` spike kept alive as integration suite |
| PNA / mixed-content in-browser policy changes | Shim's CORS breaks as default | `M1` spike matrix re-run per major browser; CORS matrix tested in CI against headless Chromium (canonical origin drift path for CI branch). |
| Sign-notarized builds missing early | Windows Defender / Gatekeeper friction | Zip-prototype path ships first; `MSI`/signed notarized binary paths reserved (missed milestones list them as wonk). |

---

## 19. Decisions log

| # | Decision | Rationale | Status |
|---|---|---|---|
| D1 | Language: Go. | Single static artefact cross-builds Flatpak/Win/mac; `tsnet` is Go-native; media proxy's `net/http` fits. Confirmed by user. | **Locked** |
| D2 | Companion is windowless (no settings GUI). Management = headless + TorrServer web UI on loopback internally. | Per user: no extra companion complexity. | **Locked** |
| D3 | Paired only if server-verified. Logged-in users only. | Per user: "app only caters logged in users". Mechanism superseded by D12 (was: companion forwards caller's Supabase token to `${SUPABASE_URL}/auth/v1/user`). | **Locked (mechanism via D12)** |
| D4 | Pairing ceremony: **explicit approval showing backend-verified identity** (not silent TOFU, not code-in-browser). | Per user: "explicit notification is better … we can show a user name based on that token so the approve notification cant trick the user". Mitigates pairing-window hijacks. Mechanism superseded by D12: approval gate moved from OS notification to logged-in webapp (stronger verification); notification kept as confirmation + Revoke. | **Locked (mechanism via D12)** |
| D5 | CORS is not authentication: pairing ceremony never relaxes CORS to `*` (Stremio comparison closed). | Confirmed: Stremio's `Access-Control-Allow-Origin: *` is not the model. Allowlist §6 is the perimeter, token is the gate. | **Locked** |
| D6 | Engine always loopback; shim is only exposed surface. TorrServer `shared` never opens a second Host. | Safety by construction. | **Locked** |
| D7 | `tsnet` as headline for shared mode (with raw-LAN warned fallback). | Makes LAN both secure and zero-firewall; optional. | **Planned (default rec)** |
| D8 | Default pending policy for new pairings when backend unreachable: **fail-closed** (410/ph). | User: logged-in-only already; paired tokens keep working offline. `pairing.requireOnlineVerification` baked in, flip-off requires user's act. | **Planned (default rec — confirm at kick-off)** |
| D9 | Android via `gomobile bind` → foreground service deferred to `M6`. | Prior-art path exists; would need new app-review attestation + wakelock policy. TorrServe covers engine-only in interim. | **Deferred** |
| D10 | Flatpak-first packaging. | User: "Starting from Linux as flatpak". | **Locked** |
| D11 | Canonical deployment's crispy-server URL + `allowedOrigins` baked as defaults; self-hosters re-point. | User: "anyone selfhosting will make changes". | **Locked** |
| D12 | Pairing protocol: **crispy-server's existing RFC 8628 device grant** with new clientId `crispy-companion` (add to `DEVICE_AUTHORIZATION_CLIENT_IDS`, `deviceType:'companion'`). Companion never sees Supabase tokens; approval happens in the logged-in webapp; desktop notification = confirmation + Revoke. Revocation via existing `DELETE /v1/auth/devices/{id}`; companion lazily re-validates its device `cp_pat_`. | crispy-server already implements the full grant (`device-authorization.service.ts`); zero new endpoints, no bearer session credential crosses the origin→loopback boundary, server-side device inventory + revocation for free. Supersedes the companion-verifies-Supabase-token mechanism. | **Locked** |

Earlier "no-surface" pairing (silent TOFU) and a raw QR-from-host code were considered and **rejected** for v1 in favour of D4. They survive as `pairing.requireOnlineVerification=false` and a future flag re-entering `M5` if wanted.

---

## 20. Open questions

> Resolved pre-code or at `M0–M1` kick-off; none block `PLAN.md` landing.

| # | Question | Type |
|---|---|---|
| Q1 | Single canonical webapp origin (or small set) + canonical crispy-server URL to bake for release? Server-side change: add `crispy-companion` to `DEVICE_AUTHORIZATION_CLIENT_IDS` + `deviceType:'companion'`. | Confirm with webapp/server repo (ties to §9.2 + §6 allowedOrigins) |
| Q2 | Exact Stremio file-list ordering (`torrentFiles.sort(path)` assumption vs aliased TorrServer ordering) | `M0` spike |
| Q3 | `streamProgress` precise formula (contiguous cache bytes from `CacheState` vs `torrent.stat_string` download fraction) to hold `streamProgress ≥ 0.05` gate seam-free | `M0` spike |
| Q4 | tajan *macOS notification with action on signed LaunchAgent without entitlements* — cgo bridge minimum vs bundled helper? | `M4` spike |
| Q5 | `tsnet` delivery: embedded as `shared.mode=tsnet` auto-goes via `tailscale/tailscale#tsnet` or flexible fallback to raw? | Bundling decision at `M5` |

---

## Appendix — Repository checklist (landing state)

- [x] `PLAN.md` — this file.
- [ ] `LICENSE` (`GPL-3.0`), `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md` — seed at tag.
- [ ] `go.mod` — after "go" decision (v1 kick-off).
- [ ] `packaging/flatpak/io.crispy.Companion.json` — `M1`+.
- [ ] `docs/spikes/M0-notes.md`, `docs/security-review.md` — `M0`/`M7`.

Anything outside this stance is a **planned addition**, not a deferred debt — flag it via `CRISPY_DESIGN_OPEN_Q` in sync with the Crispy webapp's `src/worker/handlers/streamingServer.ts` shape change (tracked as `crispy:companion-support`).

