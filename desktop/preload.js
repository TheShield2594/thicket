// The only bridge between Thicket's pages and the desktop app: a few
// calls, no Node access.
const { contextBridge, ipcRenderer } = require('electron');

contextBridge.exposeInMainWorld('thicketDesktop', {
  // Bring the window forward (a notification was clicked).
  show: () => ipcRenderer.invoke('thicket:show'),
  // Unread count for the dock or launcher badge.
  setBadge: (n) => ipcRenderer.invoke('thicket:badge', n),
  // Setup and offline pages.
  connect: (server) => ipcRenderer.invoke('thicket:connect', server),
  retry: () => ipcRenderer.invoke('thicket:retry'),
  changeServer: () => ipcRenderer.invoke('thicket:change-server'),
});
