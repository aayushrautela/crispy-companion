'use strict';

// Minimal, deliberate bridge between the shell and the remote webapp.
//
// The window loads a REMOTE page (https://web.crispytv.tech by default), so
// it must not get native powers: no fs, no shell, no ipc beyond the two
// things below. The webapp can detect the desktop shell via
// `window.crispyDesktop` and query the app version.
//
// magnet: deep links arrive as a `crispy:magnet` CustomEvent on `window`
// (dispatched from the main process); the webapp listens with
//   window.addEventListener('crispy:magnet', (e) => e.detail)

const { contextBridge, ipcRenderer } = require('electron');

contextBridge.exposeInMainWorld('crispyDesktop', {
  platform: process.platform,
  getVersion: () => ipcRenderer.invoke('crispy-desktop:version'),
});
