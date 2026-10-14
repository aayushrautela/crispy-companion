# M0 spike notes — TorrServer engine mapping (verified live)

Date: 2026-10-14 · Engine: TorrServer **MatriX.146.1** (`TorrServer-linux-arm64`)
Test torrent: Ubuntu 24.04.3 desktop `d160b8d8ea35a5b4e52837468fc8f03d55cef1f7`
Companion: M3 handlers against the live engine on `127.0.0.1:8090`.

## API surface (all confirmed by request, plus source read of the 146.1 tree)

| Call | Notes |
|---|---|
| `POST /torrents {action:add, link}` | `link` accepts magnet / raw 40-hex / http(s) `.torrent` URL. Returns `TorrentStatus` immediately (`stat 0`, `"Torrent added"`); metadata fetch is async. |
| `POST /torrents {action:get, hash}` | `TorrentStatus` incl. `file_stats`; **404** when never-added or dropped. |
| `POST /cache {action:get, hash}` | `CacheState {PiecesLength, PiecesCount, Pieces: {n: {Completed,…}}}`; **404** when dropped. Empty `Pieces` before download starts. |
| `GET /stream?link=<hash>&index=<1-based>&play` | `link` accepts the **raw hash**; reuses the existing torrent (no re-add). `index` is **1-based** (`index=1` for single-file). Bytes served by `http.ServeContent` → full `Range`/`If-Range`/`ETag`/206 support, `Content-Type` by extension. |

## Findings that shaped M3

1. **Hash must be valid 40-hex.** An early spike failure (`unhandled xt parameter encoding (encoded length 41)`) was a malformed 41-char test hash, not an engine bug — traced through the vendored `tsynik/torrent` fork's magnet parser to confirm.
2. **File IDs are 1-based over the path-sorted list** (`Id: i+1 // in web id 0 is undefined`, sorted by path). Mapping `engineID = fileIdx+1` with bounds check; ordering divergence vs Stremio archive-order indices stays open as PLAN Q2 (stats carries `engineFileId` + names for future reconciliation).
3. **Idle drop is real:** zero readers + `TorrentDisconnectTimeout` (30s default) → `get`/`cache` 404 (`expired()` in `torr/torrent.go`). The shim re-adds transparently from the remembered link (verified in tests + live: stats after drop re-adds exactly once).
4. **Hash-only magnets need trackers.** `magnet:?xt=urn:btih:{ih}` alone leans on DHT; the shim enriches with public trackers (opentrackr, stealth) — Ubuntu metadata arrived in ~20s.
5. **Progress formula** (byte-precise, verified live): file offset from the sorted `file_stats`, piece overlap via `PiecesLength` + `Pieces[n].Completed`. Live: after pulling 1 MiB through the proxy, stats reported `streamProgress ≈ 0.00219`, `downloaded = 13893632` (13 MiB — read-ahead working).
6. **Proxy parity** (the CORS fix): 1 MiB `Range` through the companion is **bit-identical** (`cmp`) to direct-engine bytes, with `206` + `Content-Range` passthrough.

## PNA / mixed-content matrix (still open, needs browser pass)

Shim sends `Access-Control-Allow-Private-Network: true` + exact-origin reflection (unit-tested in `httpx`). The real-browser pass (Chrome/Firefox/Safari over the canonical HTTPS origin → `http://127.0.0.1:11470`) is deferred to M7 hardening — needs the webapp integration (Crispy-side items in PLAN §14) to exist first.
