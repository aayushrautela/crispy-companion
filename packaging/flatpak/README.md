# Flatpak packaging (M4)

Primary distribution for Linux. Headless daemon; the `.desktop` entry is
`NoDisplay` (autostart + `pair show` CLI only).

## Permissions rationale

| Permission | Why |
|---|---|
| `--share=network` | Bind `:11470` loopback + reach trackers/peers + crispy-server TLS. Required. Ports >1024 need nothing else. |
| `--talk-name=org.freedesktop.portal.Desktop` | Pairing/result notifications with Revoke action (Notification portal). |
| `--talk-name=org.freedesktop.portal.Background` | Optional autostart (`--request-autostart`); user revocable in Settings. |
| `--filesystem=xdg-data/crispy-companion` | The ONLY host filesystem access: tokens, config, engine cache. No `home:`, no `host:`. |

No X11/Wayland sockets: there is no GUI. No `--device=all`, no DRI.

## Build

```sh
# From the repo root. Fill the two sha256 pins first (see below).
flatpak-builder --force-clean --repo=repo build-dir packaging/flatpak/io.crispy.Companion.json
flatpak build-bundle repo crispy-companion.flatpak io.crispy.Companion
```

Vendored Go build (`GOFLAGS=-mod=vendor`, `CGO_ENABLED=0`): no network at
build time, satisfying Flathub requirements. TorrServer binaries are pinned
`file` sources (MatriX.146.1) with sha256 — replace
`REPLACE_WITH_AMD64_SHA256` / `REPLACE_WITH_ARM64_SHA256` with the real
hashes from the
[TorrServer release page](https://github.com/YouROK/TorrServer/releases)
(checksums are published per artefact; verify out-of-band before pinning).

## Autostart

```sh
flatpak run io.crispy.Companion --request-autostart
```

One-shot: requests Background portal autostart, then serves. Revoke any
time in desktop Settings → Apps → Crispy Companion.

## Pairing without notifications

If the notification (with its Revoke button) is missed:

```sh
flatpak run io.crispy.Companion pair show
```

Prints the pending user code + verification URL (no secrets — the device
code never leaves companion memory). Approve in the logged-in Crispy
web app; the browser polling `/status?pendingPairId=` receives the token.

## Firewall note

Flatpak cannot open host firewall ports and the default `loopback` mode
needs none. `shared: raw` LAN mode is the only case that needs a manual
`firewalld`/`ufw` opening — documented at pair time, never automatic.

## Status: build not yet run

No flatpak-builder in this dev container, so the manifest is review-ready
but uncompiled. First real build + portal smoke (notification action,
background request, `pair show`) is the M4 acceptance gate on a desktop host.
