'use strict';

// Crispy desktop shell — Electron main process.
//
// One app, two parts:
//   1. the Crispy webapp, loaded from a REMOTE URL (compiled-in default
//      https://web.crispytv.tech, overridable via CRISPY_DESKTOP_URL).
//      The website stays remote on purpose — nothing is bundled.
//   2. the crispy-companion Go daemon on loopback (127.0.0.1:11470), spawned
//      here at startup unless one is already running (a standalone
//      companion install keeps working; we just reuse it).
//
// The webapp talks to the companion over loopback HTTP exactly like in a
// plain browser: Chromium treats 127.0.0.1 as potentially trustworthy and
// the companion answers the Private Network Access preflight with
// Access-Control-Allow-Private-Network: true (PLAN.md §10), so this shell
// needs no CORS shim of its own.
//
// Security posture: the window shows remote content, so it gets zero
// native capabilities — nodeIntegration off, sandbox on, contextIsolation
// on, and the preload only exposes { platform, getVersion(), getCompanionToken() }.
// Navigation away from the webapp origin is redirected to the system browser.
//
// Pairing: when this shell spawns the companion it sets
// CRISPY_COMPANION_ALLOW_SHELL_TOKEN=1, and the daemon then answers
// POST /companion/shell-token to non-browser local callers only (the handler
// rejects any request carrying Origin or Sec-Fetch-Site, so no web page can
// ever mint a token). The shell fetches that token once and the preload hands
// it to the webapp, which stores it as its companion token — the bundled app
// is paired without the RFC 8628 device-grant prompt. Reusing an already
// running standalone companion yields no token (404), and the webapp falls
// back to manual pairing, exactly like a plain browser.

const { app, BrowserWindow, Menu, dialog, ipcMain, shell } = require('electron');
const { autoUpdater } = require('electron-updater');
const { spawn } = require('node:child_process');
const fs = require('node:fs');
const path = require('node:path');

const DEFAULT_WEBAPP_URL = 'https://web.crispytv.tech';
const MAGNET_SCHEME = 'magnet';

// The companion origin is derived from the same env vars the daemon reads,
// so a port override applies to both sides of the pair.
const COMPANION_HOST = process.env.CRISPY_COMPANION_LISTEN_ADDR || '127.0.0.1';
const COMPANION_PORT = process.env.CRISPY_COMPANION_LISTEN_PORT || '11470';
const COMPANION_ORIGIN = `http://${COMPANION_HOST}:${COMPANION_PORT}`;
const PING_URL = `${COMPANION_ORIGIN}/companion/ping`;

let LOG_PATH = null;
let mainWindow = null;
let webappOrigin = null;
let companionProc = null;
let companionRestarts = 0;
let quitting = false;
let shellToken = null;
const pendingMagnets = [];

function log(...args) {
  const line = `[${new Date().toISOString()}] ${args.map(String).join(' ')}`;
  console.log('[crispy-desktop]', ...args);
  if (LOG_PATH) {
    try {
      fs.appendFileSync(LOG_PATH, line + '\n');
    } catch {
      /* logging must never crash the shell */
    }
  }
}

// --- webapp URL -----------------------------------------------------------

function resolveWebappURL() {
  const raw = (process.env.CRISPY_DESKTOP_URL || DEFAULT_WEBAPP_URL).trim();
  let u;
  try {
    u = new URL(raw);
  } catch {
    throw new Error(`CRISPY_DESKTOP_URL is not a valid URL: ${raw}`);
  }
  const loopback =
    u.hostname === '127.0.0.1' || u.hostname === 'localhost' || u.hostname === '[::1]';
  if (u.protocol !== 'https:' && !(u.protocol === 'http:' && loopback)) {
    throw new Error(
      `CRISPY_DESKTOP_URL must be https: (http: only on loopback for dev), got ${u.protocol}//${u.host}`
    );
  }
  return u;
}

// --- companion lifecycle ----------------------------------------------------

function resolveCompanionBinary() {
  if (process.env.CRISPY_COMPANION_BIN) return process.env.CRISPY_COMPANION_BIN;
  const exe = process.platform === 'win32' ? 'crispy-companion.exe' : 'crispy-companion';
  if (app.isPackaged) {
    const p = path.join(process.resourcesPath, 'bin', exe);
    if (fs.existsSync(p)) return p;
  } else {
    // dev: repo-root bin/ (run `make build` first)
    const p = path.join(__dirname, '..', '..', 'bin', exe);
    if (fs.existsSync(p)) return p;
  }
  // fall back to PATH: a standalone companion install
  return exe;
}

async function pingCompanion(timeoutMs = 1500) {
  try {
    const res = await fetch(PING_URL, { signal: AbortSignal.timeout(timeoutMs) });
    return res.ok;
  } catch {
    return false;
  }
}

async function waitForCompanion(ms) {
  const deadline = Date.now() + ms;
  while (Date.now() < deadline) {
    if (await pingCompanion(1000)) return true;
    await new Promise((r) => setTimeout(r, 250));
  }
  return false;
}

async function ensureCompanion() {
  if (await pingCompanion()) {
    log(`companion already running at ${COMPANION_ORIGIN} — reusing it`);
    return;
  }
  const bin = resolveCompanionBinary();
  log('starting companion:', bin);
  const logFd = LOG_PATH ? fs.openSync(LOG_PATH, 'a') : 'ignore';
  try {
    companionProc = spawn(bin, [], {
      env: { ...process.env, CRISPY_COMPANION_ALLOW_SHELL_TOKEN: '1' },
      stdio: ['ignore', logFd, logFd],
      windowsHide: true,
    });
  } catch (err) {
    log('failed to spawn companion:', err.message);
    dialog.showErrorBox(
      'Crispy companion failed to start',
      `Could not start the local companion.\n\nBinary: ${bin}\nError: ${err.message}\n\nPlayback will not work until the companion runs on ${COMPANION_ORIGIN}.`
    );
    return;
  }
  companionProc.on('exit', (code, sig) => {
    log('companion exited', { code, sig });
    companionProc = null;
    if (!quitting && companionRestarts < 3) {
      companionRestarts += 1;
      log(`respawning companion (attempt ${companionRestarts}/3)`);
      setTimeout(() => ensureCompanion(), 2000);
    }
  });
  if (await waitForCompanion(15000)) {
    log(`companion is up: ${PING_URL}`);
    return;
  }
  log('companion did not answer ping within 15s');
  dialog.showErrorBox(
    'Crispy companion failed to start',
    `The local companion at ${COMPANION_ORIGIN} did not respond within 15s.\n\nBinary: ${bin}\n\nPlayback will not work until it runs. See the log at ${LOG_PATH}.`
  );
}

// Fetch the shell token from the daemon. undici sends neither Origin nor
// Sec-Fetch-Site, so the daemon's non-browser gate accepts this call; a
// reused standalone companion (flag unset) answers 404 and we return null.
async function fetchShellToken() {
  try {
    const res = await fetch(`${COMPANION_ORIGIN}/companion/shell-token`, {
      method: 'POST',
      headers: { 'X-Crispy-Client': `crispy-desktop/${app.getVersion()}` },
      signal: AbortSignal.timeout(3000),
    });
    if (!res.ok) return null;
    const body = await res.json();
    return typeof body.token === 'string' && body.token ? body.token : null;
  } catch {
    return null;
  }
}

// --- magnet: deep links ------------------------------------------------------

function findMagnet(argv) {
  return argv.find((a) => typeof a === 'string' && a.startsWith(`${MAGNET_SCHEME}:`));
}

function deliverMagnet(url) {
  if (!mainWindow || mainWindow.isDestroyed()) return;
  if (mainWindow.webContents.isLoading()) {
    pendingMagnets.push(url);
    return;
  }
  mainWindow.webContents
    .executeJavaScript(
      `window.dispatchEvent(new CustomEvent('crispy:magnet', { detail: ${JSON.stringify(url)} }));`
    )
    .catch((err) => log('magnet delivery failed:', err.message));
}

function registerMagnetScheme() {
  try {
    if (process.defaultApp) {
      // dev (`electron .`): register the dev binary so `xdg-open magnet:…` works
      app.setAsDefaultProtocolClient(MAGNET_SCHEME, process.execPath, [path.resolve(process.argv[1])]);
    } else {
      app.setAsDefaultProtocolClient(MAGNET_SCHEME);
    }
  } catch (err) {
    // Flatpak: the shipped .desktop (MimeType=x-scheme-handler/magnet, Exec with
    // %u) covers portal-based launches; a runtime registration failure is fine.
    log('magnet scheme registration skipped:', err.message);
  }
}

// --- window ------------------------------------------------------------------

function createWindow(webappURL) {
  webappOrigin = new URL(webappURL).origin;
  log(`webapp origin: ${webappOrigin} (must be in the companion's allowed origins — CRISPY_COMPANION_ALLOWED_ORIGINS)`);

  mainWindow = new BrowserWindow({
    width: 1280,
    height: 800,
    minWidth: 940,
    minHeight: 600,
    title: 'Crispy',
    autoHideMenuBar: true,
    backgroundColor: '#0b0b0d',
    webPreferences: {
      preload: path.join(__dirname, 'preload.js'),
      contextIsolation: true,
      nodeIntegration: false,
      sandbox: true,
    },
  });

  // Keep the window on the webapp origin; everything else goes to the
  // system browser.
  mainWindow.webContents.on('will-navigate', (event, url) => {
    if (new URL(url).origin !== webappOrigin) {
      event.preventDefault();
      shell.openExternal(url).catch(() => {});
    }
  });
  mainWindow.webContents.setWindowOpenHandler(({ url }) => {
    if (new URL(url).origin === webappOrigin) return { action: 'allow' };
    shell.openExternal(url).catch(() => {});
    return { action: 'deny' };
  });

  mainWindow.webContents.on('did-finish-load', () => {
    const queued = pendingMagnets.splice(0);
    for (const m of queued) deliverMagnet(m);
  });

  mainWindow.on('closed', () => {
    mainWindow = null;
  });

  mainWindow.loadURL(webappURL).catch((err) => {
    log('failed to load webapp:', err.message);
    dialog.showErrorBox(
      'Could not load the Crispy webapp',
      `${webappURL}\n\n${err.message}\n\nCheck the connection, or point CRISPY_DESKTOP_URL at another deployment.`
    );
  });
}

// --- updates -------------------------------------------------------------------

function maybeCheckForUpdates() {
  if (!app.isPackaged) return;
  if (process.env.FLATPAK_ID) return; // Flathub owns Flatpak updates
  if (process.platform !== 'win32' && process.platform !== 'darwin' && !process.env.APPIMAGE) return;
  try {
    autoUpdater.autoDownload = true;
    autoUpdater.checkForUpdatesAndNotify().catch((err) => log('updater:', err.message));
  } catch (err) {
    log('updater init failed:', err.message);
  }
}

// --- startup -------------------------------------------------------------------

const gotLock = app.requestSingleInstanceLock();
if (!gotLock) {
  app.quit();
} else {
  app.on('second-instance', (_event, argv) => {
    const magnet = findMagnet(argv);
    if (mainWindow) {
      if (mainWindow.isMinimized()) mainWindow.restore();
      mainWindow.focus();
      if (magnet) deliverMagnet(magnet);
    }
  });

  app.on('open-url', (event, url) => {
    event.preventDefault();
    if (url.startsWith(`${MAGNET_SCHEME}:`)) deliverMagnet(url);
  });

  app.on('will-quit', () => {
    quitting = true;
    if (companionProc) {
      try {
        companionProc.kill();
      } catch {
        /* already gone */
      }
      companionProc = null;
    }
  });

  app.on('window-all-closed', () => {
    app.quit();
  });

  app.on('activate', () => {
    if (BrowserWindow.getAllWindows().length === 0 && webappOrigin) {
      createWindow(webappOrigin + '/');
    }
  });

  ipcMain.handle('crispy-desktop:version', () => app.getVersion());
  // The local companion token minted by the shell-spawned daemon; null when
  // no shell-spawned daemon is in use (webapp pairs manually then).
  ipcMain.handle('crispy-desktop:companion-token', () => shellToken);

  app.whenReady().then(async () => {
    LOG_PATH = path.join(app.getPath('userData'), 'main.log');
    log('crispy-desktop', app.getVersion(), 'starting; packaged =', app.isPackaged);

    let webappURL;
    try {
      webappURL = resolveWebappURL().toString();
    } catch (err) {
      dialog.showErrorBox('Invalid CRISPY_DESKTOP_URL', err.message);
      app.quit();
      return;
    }

    if (process.platform !== 'darwin') Menu.setApplicationMenu(null);

    await ensureCompanion();
    shellToken = await fetchShellToken();
    log(
      shellToken
        ? 'got shell token from companion — webapp will be paired as crispy-desktop'
        : 'no shell token (standalone companion reused?) — webapp will pair manually'
    );
    registerMagnetScheme();
    createWindow(webappURL);
    maybeCheckForUpdates();

    const startupMagnet = findMagnet(process.argv);
    if (startupMagnet) deliverMagnet(startupMagnet);
  });
}
