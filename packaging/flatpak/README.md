# Flatpak packaging (M4)

Primary distribution for Linux. Headless daemon with a start-menu entry:
the `.desktop` file is exported to the application menu and reports the
daemon's state through desktop notifications (autostart + `pair show` CLI
as well).

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

## Start menu entry and app icon

The desktop entry is exported to the host's application menu by Flatpak.
It launches the daemon in menu mode (`--menu-launch`); because the daemon
is headless, it answers every launch with a desktop notification:

| Situation | Notification |
|---|---|
| Fresh start | "Crispy Companion started" |
| Clicked again while running | "Crispy Companion is already running" (no second engine is spawned) |

The entry is `packaging/flatpak/io.crispy.Companion.desktop`; it must keep
`NoDisplay` off or the app stays hidden from the menu, and `Icon=` must
equal the app ID so Flatpak exports the icon to the host.

### Icon formats

The icon is the **Crispy brand mark** (black disc, red play wedge, white
outlined chevron), taken from the Crispy web app's icon set so the daemon
reads as part of the same product. The icon name is always the app ID:
`io.crispy.Companion`. Two accepted layouts (the SVG is the primary one;
the PNGs are the raster fallbacks):

```
packaging/flatpak/io.crispy.Companion.svg            # hicolor/scalable
packaging/flatpak/icons/<size>x<size>/io.crispy.Companion.png
```

The PNG set is committed (16, 24, 32, 48, 64, 128, 256, 512). It was
rasterized from the web app's own 512×512 master, so the rasters are
pixel-identical to the brand artwork.

Requirements for the SVG:

- **the file must start with `<svg`** — gdk-pixbuf's SVG loader detects SVGs
  by the magic ` <svg` / ` <!DOCTYPE svg`, so an XML declaration
  (`<?xml …?>`) or a comment *before* the root element makes the loader
  ignore the file. AppStream then fails the Flatpak build with
  `E: icon-file-read-error` and `E: filters-but-no-output` — the bundles
  never get built. CI checks the first bytes for you.
- square `viewBox` (128×128 or 512×512), no external references, no
  embedded raster data, transparent background
- no text that depends on installed fonts

Requirements for the PNGs (if you ship them):

- real PNG files (not renamed SVGs), correct pixel dimensions, transparency
- useful sizes: 16, 24, 32, 48, 64, 128, 256, 512

The manifest installs every PNG found under `packaging/flatpak/icons/` into
`hicolor/<size>x<size>/apps/` and the SVG into `hicolor/scalable/apps/`.
`appstreamcli compose` then exports all of them (scalable icon plus every
raster size) and rasterizes the SVG into the cached appstream icon sizes
(120, 150, 180 …), so the app menu shows the brand mark at any size.
To regenerate the PNGs from the SVG:

```sh
scripts/render-icons.sh          # needs rsvg-convert, inkscape or ImageMagick
```

## Deployment defaults (.env in the tree, secrets in CI, never in source)

Our domains live outside the source tree: locally in `.env` (gitignored,
see `.env.example`), in CI as repository secrets. `scripts/builddefaults.sh`
reads them and writes the gitignored
`internal/config/builddefaults_gen.go`, which the Flatpak manifest picks up
as part of the source tree:

| Key | Purpose |
|---|---|
| `CRISPY_SERVER_URL` | Where the companion phones home to pair (`https://api.crispytv.tech`) |
| `CRISPY_ALLOWED_ORIGINS` | Web origins allowed to reach the loopback API (`https://web.crispytv.tech`) |

Locally:

```sh
cp .env.example .env && $EDITOR .env   # then build
```

In CI, set the same two names as repository secrets (Settings → Secrets
and variables → Actions), or:

```sh
gh secret set CRISPY_SERVER_URL --body 'https://api.crispytv.tech'
gh secret set CRISPY_ALLOWED_ORIGINS --body 'https://web.crispytv.tech'
```

Notes:

- These are **deployment values, not cryptographic secrets**: every shipped
  bundle must contain them in readable form to function. Secrets are used to
  keep infra URLs out of the public source tree and masked in build logs.
- Builds without the values (forks, PRs) skip injection and keep the
  fail-closed OSS defaults (no server URL, example origins only).
- A user's own `config.toml` or `CRISPY_COMPANION_*` env vars always
  override the compiled-in values.
- **Always use `https://`.** The device-grant poll carries the device code
  and its reply carries the `cp_pat_`, so neither may travel in cleartext;
  `internal/config` therefore upgrades a non-loopback `http://` server URL
  to `https://` on its own. The grant client also replays the request body
  across an `http→https` redirect (Go would otherwise turn the POST into a
  GET and the API would answer "must have required property 'clientId'").

## Status: manifest CI-verified, desktop smoke test pending

`flatpak-builder` is not available in this dev container, so the first real
compile happens in CI (`.github/workflows/flatpak.yml`). First desktop-host
portal smoke (notification action, background request, `pair show`) remains
the M4 acceptance gate.
