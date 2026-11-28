import path from 'node:path';
import { origin, scheme, preferences, route } from './policy.mjs';
import { installIPC } from './ipc.mjs';

// One non-persistent Chromium session per selected instance. All content comes
// from its checked daemon connection, with the daemon's CSP unchanged.
export async function createWindow(electron, transport, preload) {
  const { BrowserWindow, session, ipcMain } = electron;
  const privateSession = session.fromPartition('native-' + crypto.randomUUID());
  privateSession.setPermissionRequestHandler((_contents, _permission, reply) => reply(false));
  privateSession.setPermissionCheckHandler(() => false);
  privateSession.setDevicePermissionHandler(() => false);
  privateSession.on('will-download', event => event.preventDefault());
  privateSession.webRequest.onBeforeRequest((details, reply) => {
    try { route(details.url, details.method, false, transport.setup); reply({ cancel: false }); }
    catch { reply({ cancel: true }); }
  });
  await privateSession.protocol.handle(scheme, request => transport.fetch(request));
  const window = new BrowserWindow({ width: 1280, height: 850, show: false,
    webPreferences: { ...preferences, session: privateSession, preload: path.resolve(preload) } });
  const contents = window.webContents;
  const dispose = installIPC(ipcMain, contents, transport);
  const navigation = (event, target) => {
    try {
      const url = new URL(target);
      if (url.protocol !== scheme + ':' || url.host !== 'instance' || url.pathname !== '/' || url.username || url.password) event.preventDefault();
    } catch { event.preventDefault(); }
  };
  contents.on('will-navigate', navigation);
  contents.on('will-redirect', event => event.preventDefault());
  contents.on('will-frame-navigate', navigation);
  contents.on('will-attach-webview', event => event.preventDefault());
  contents.setWindowOpenHandler(() => ({ action: 'deny' }));
  contents.on('render-process-gone', () => transport.close());
  window.on('closed', dispose);
  await window.loadURL(origin + '/');
  return window;
}
