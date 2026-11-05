'use strict';

// Minimal, deliberate bridge between the shell and the remote webapp.
//
// The window loads a REMOTE page (https://web.crispytv.tech by default), so
// it must not get native powers: no fs, no shell, no ipc beyond the things
// below. The webapp can detect the desktop shell via `window.crispyDesktop`.
//
// magnet: deep links arrive as a `crispy:magnet` CustomEvent on `window`
// (dispatched from the main process); the webapp listens with
//   window.addEventListener('crispy:magnet', (e) => e.detail)

const { contextBridge, ipcRenderer } = require('electron');

contextBridge.exposeInMainWorld('crispyDesktop', {
  platform: process.platform,
  getVersion: () => ipcRenderer.invoke('crispy-desktop:version'),
  // Local companion token minted by the shell-spawned daemon (device
  // "crispy-desktop"). The webapp stores it as its pairing token, so the
  // bundled app never shows the device-grant pairing prompt. Null when the
  // shell reuses a standalone companion — then pairing is manual, as in a
  // plain browser.
  getCompanionToken: () => ipcRenderer.invoke('crispy-desktop:companion-token'),
});
