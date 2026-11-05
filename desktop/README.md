# Crispy desktop shell (Electron)

One standalone desktop app = the **Crispy webapp (remote, over HTTPS)** in a
native window + the **crispy-companion Go daemon** bundled underneath. The
user clicks one icon and gets the app; the daemon stays a normal loopback
HTTP service on `127.0.0.1:11470`, so the plain-browser workflow keeps
working unchanged. **The Go daemon is not modified by this shell** — it is
spawned as a child process.

Why Electron (decision, 2026-11): the webapp page is HTTPS and calls
`http://127.0.0.1:11470`. Chromium (Electron on all three OSes, and WebView2
on Windows) treats loopback as potentially trustworthy and only asks the PNA
preflight, which the companion answers (`Access-Control-Allow-Private-Network:
true` + exact-origin CORS reflection, PLAN.md §10). WebKit (Tauri on macOS /
WebKitGTK on Linux) does **not** treat loopback as trustworthy and blocks the
fetch as mixed content (WebKit bug 171934, still open), so Tauri would have
needed a shell-side proxy on 2 of 3 platforms. Electron needs zero workarounds.

## How it works

`src/main.js` on startup:

1. Takes the single-instance lock (second launch focuses the window; a
   `magnet:` argument is forwarded into the page).
2. Pings `GET http://127.0.0.1:11470/companion/ping`. If a companion already
   answers (e.g. the standalone install autostarted at login), it is reused.
   Otherwise the bundled `crispy-companion` binary is spawned (its stderr is
   appended to the shell log), and we wait for the ping. If it dies, it is
   respawned up to 3 times. On quit, the shell kills the daemon **only if it
   spawned it**.
3. Opens one `BrowserWindow` loading the webapp URL:
   `CRISPY_DESKTOP_URL` env override, else the compiled-in default
   `https://web.crispytv.tech`. Only `https:` is accepted (loopback `http:`
   allowed for dev). Icon/start-menu launches use the default; terminal
   users can override with the env var.
   > Any override origin must also be present in the companion's allowed
   > origins (`CRISPY_COMPANION_ALLOWED_ORIGINS` / build-time
   > `CRISPY_ALLOWED_ORIGINS`), or the companion 403s the page.
4. Registers the `magnet:` scheme (packaged Linux: the shipped `.desktop`
   with `MimeType=x-scheme-handler/magnet` and `Exec=… %u` covers it).
5. Checks for app updates via `electron-updater` on Windows/macOS/AppImage —
   **not** on Flatpak (Flathub owns updates there).

The window loads a **remote** page, so it gets zero native powers:
`nodeIntegration: false`, `sandbox: true`, `contextIsolation: true`.
`src/preload.js` exposes only `window.crispyDesktop = { platform, getVersion(),
getCompanionToken() }` so the webapp can detect the desktop shell. Navigations
and `window.open` to other origins are handed to the system browser.

### Seamless pairing (no device-grant prompt in the bundled app)

When the shell spawns the companion it sets `CRISPY_COMPANION_ALLOW_SHELL_TOKEN=1`.
The daemon then answers `POST /companion/shell-token` — but only to non-browser
local callers: the handler rejects any request carrying `Origin` or a
`Sec-Fetch-Site` other than `none`, so a web page on an allowlisted origin can
ping but never mint a token. The shell calls it once from the main process
(undici sends neither header) and exposes the token via
`window.crispyDesktop.getCompanionToken()`. The webapp stores it as its
companion token (`applyDesktopShellToken()` in `src/services/companion.ts`),
so the bundled app is paired instantly and the pairing banner never shows.
If the shell reuses an already-running standalone companion (flag unset), the
endpoint 404s, the bridge returns null, and the webapp falls back to the normal
manual pairing — same as a plain browser.

### Webapp integration points (webapp repo)

- Detect the shell: `if (window.crispyDesktop) …`
- Adopt the shell-spawned companion token: `await window.crispyDesktop.getCompanionToken()`
  (handled by `applyDesktopShellToken()` in `src/services/companion.ts`).
- Receive deep links: `window.addEventListener('crispy:magnet', (e) => e.detail)`
  — `e.detail` is the full `magnet:…` URL. Delivered on launch and on
  second-instance/`open-url`.

## Environment variables

| Var | Default | Purpose |
|---|---|---|
| `CRISPY_DESKTOP_URL` | `https://web.crispytv.tech` | Webapp URL the window loads. https: only (loopback http: for dev). |
| `CRISPY_COMPANION_BIN` | bundled binary, else repo `bin/`, else `PATH` | Companion binary to spawn when none is running. |
| `CRISPY_COMPANION_LISTEN_ADDR` / `CRISPY_COMPANION_LISTEN_PORT` | `127.0.0.1` / `11470` | Same vars the daemon reads; the shell pings the same origin. |

All `CRISPY_COMPANION_*` vars are passed through to the spawned daemon.

## Dev

```sh
make build                 # repo root: builds bin/crispy-companion
cd desktop
npm install
npm start                  # or: CRISPY_DESKTOP_URL=http://127.0.0.1:5173 npm start
```

Shell log: `app.getPath('userData')/main.log`
(`~/.config/Crispy/main.log` on Linux, `%APPDATA%/Crispy/main.log` on Windows,
`~/Library/Logs/Crispy/main.log` on macOS).

## Packaging

The companion binary is bundled via `build.extraResources` from the repo
`bin/` dir — **run `make build` before `npm run dist`**.

- **Windows**: `npm run dist:win` → NSIS installer (`Crispy-Setup-…exe`),
  per-user install. electron-updater needs a `publish` provider configured
  before auto-update works (add to `build.publish` at release time).
- **macOS**: `npm run dist:mac` → DMG. Notarization via `notarytool` to be
  wired in CI.
- **Linux**: primary format is the **Flatpak** (below); AppImage via
  `npm run dist:linux` for non-Flatpak users.

### Flatpak (`desktop/packaging/flatpak/`)

`io.crispy.Crispy` manifest: freedesktop runtime 25.08 + golang & nodejs SDK
extensions; builds the Go companion and the pinned TorrServer into `/app/bin`,
installs the pinned Electron zip into `/app/lib/crispy-desktop`, and the app
into `/app/share/crispy-desktop/app`. Launch via the `crispy` wrapper, which
runs Electron with `--no-sandbox` (the setuid chrome-sandbox cannot work
inside Flatpak; the Flatpak sandbox is the isolation).

finish-args: `--share=network` (companion + engine), wayland/x11/ipc sockets,
`--device=dri` (HW video decode), the Desktop/Background/Notification portal
talk-names, and `--filesystem=xdg-data/crispy-companion:create` so the desktop
app and the daemon-only Flatpak share pairing state in the same XDG data dir.

Build locally from the repo root:

```sh
flatpak-builder --user --install --force-clean _build desktop/packaging/flatpak/io.crispy.Crispy.json
flatpak run io.crispy.Crispy
```

Notes for Flathub submission: pin the Electron zip `sha256` in the manifest,
replace the `type: dir` app source with `flatpak-node-generator` output, and
keep the `.desktop` `Exec=crispy %u` (the `%u` is what delivers `magnet:`
URLs through portals).

## Open product decisions (not wired yet)

- Close-window behavior: currently **quit**. Minimize-to-tray and
  login-autostart of the desktop app itself (Background portal entry with
  `Exec=flatpak run io.crispy.Crispy`) are follow-ups.
- The daemon still arranges its own login autostart; the desktop app
  ping-first reuse makes that harmless (one daemon, whichever started first).
