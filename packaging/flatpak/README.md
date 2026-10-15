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
# From the repo root:
flatpak-builder --force-clean --repo=repo build-dir packaging/flatpak/io.crispy.Companion.json
flatpak build-bundle repo crispy-companion.flatpak io.crispy.Companion
```

CI builds both arches on every push/PR via `.github/workflows/flatpak.yml`
(`x86_64` + `aarch64`, bundles uploaded as artifacts; tagged `v*` releases
get the `.flatpak` files attached automatically).

Vendored Go build (`GOFLAGS=-mod=vendor`, `CGO_ENABLED=0`): no network at
build time, satisfying Flathub requirements. TorrServer binaries are pinned
`file` sources (MatriX.146.1) with sha256, verified against the
[TorrServer release page](https://github.com/YouROK/TorrServer/releases/tag/MatriX.146.1)
(sizes: amd64 `63373472` B, arm64 `60948640` B — must match before pinning).
To bump the engine: update both URLs + sha256 pins in
`io.crispy.Companion.json` together.

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

## Deployment defaults (GitHub Secrets → CI-injected, never in source)

Our domains live in repository secrets, not in code. On every build the
workflow writes them into the gitignored
`internal/config/builddefaults_gen.go`, which the Flatpak manifest picks up
as part of the source tree:

| Secret | Purpose |
|---|---|
| `CRISPY_SERVER_URL` | Where the companion phones home to pair (`http://api.crispytv.tech`) |
| `CRISPY_ALLOWED_ORIGINS` | Web origins allowed to reach the loopback API (`https://web.crispytv.tech`) |

Set/rotate at: repo Settings → Secrets and variables → Actions, or:

```sh
gh secret set CRISPY_SERVER_URL --body 'http://api.crispytv.tech'
gh secret set CRISPY_ALLOWED_ORIGINS --body 'https://web.crispytv.tech'
```

Notes:

- These are **deployment values, not cryptographic secrets**: every shipped
  bundle must contain them in readable form to function. Secrets are used to
  keep infra URLs out of the public source tree and masked in build logs.
- Builds without the secrets (forks, PRs) skip injection and keep the
  fail-closed OSS defaults (no server URL, example origins only).
- A user's own `config.toml` or `CRISPY_COMPANION_*` env vars always
  override the compiled-in values.

## Status: manifest CI-verified, desktop smoke test pending

`flatpak-builder` is not available in this dev container, so the first real
compile happens in CI (`.github/workflows/flatpak.yml`). First desktop-host
portal smoke (notification action, background request, `pair show`) remains
the M4 acceptance gate.
