import assert from 'node:assert/strict';
import { test } from 'node:test';
import { EventEmitter } from 'node:events';
import { createWindow } from '../src/window.mjs';
import { origin } from '../src/policy.mjs';

test('window installs private session, CSP-preserving scheme and renderer refusals', async () => {
  let prefs, filter, handler, permission, check, device, newWindow;
  const session = Object.assign(new EventEmitter(), {
    setPermissionRequestHandler: f => { permission = f; },
    setPermissionCheckHandler: f => { check = f; },
    setDevicePermissionHandler: f => { device = f; },
    webRequest: { onBeforeRequest: f => { filter = f; } },
    protocol: { handle: async (_scheme, f) => { handler = f; } },
  });
  const ipc = { handle() {}, removeHandler() {} };
  class Window extends EventEmitter {
    constructor(options) {
      super(); prefs = options.webPreferences;
      this.webContents = Object.assign(new EventEmitter(), {
        mainFrame: { url: origin + '/' },
        setWindowOpenHandler: f => { newWindow = f; },
      });
    }
    async loadURL(url) { assert.equal(url, origin + '/'); }
  }
  const original = new Response('page', { headers: { 'content-security-policy': "default-src 'self'" } });
  const window = await createWindow({ BrowserWindow: Window, ipcMain: ipc, session: { fromPartition(name) {
    assert.ok(name.startsWith('native-') && !name.startsWith('persist:')); return session;
  } } }, { fetch: async () => original, close() {} }, '/fixture/preload.cjs');
  assert.equal(prefs.nodeIntegration, false);
  assert.equal(prefs.contextIsolation, true);
  assert.equal(prefs.sandbox, true);
  assert.equal(prefs.webSecurity, true);
  assert.equal(prefs.webviewTag, false);
  assert.equal(await handler(new Request(origin + '/')), original);
  for (const url of ['https://example.com/', 'file:///fixture', 'http://localhost/api/me', origin + '/api/session']) {
    filter({ url, method: 'GET' }, result => assert.equal(result.cancel, true));
  }
  filter({ url: origin + '/api/me', method: 'GET' }, result => assert.equal(result.cancel, false));
  permission(null, 'notifications', allowed => assert.equal(allowed, false));
  assert.equal(check(), false);
  assert.equal(device(), false);
  assert.deepEqual(newWindow({ url: 'https://example.com' }), { action: 'deny' });
  let refused = 0;
  const event = { preventDefault() { refused++; } };
  window.webContents.emit('will-navigate', event, 'https://example.com/');
  window.webContents.emit('will-frame-navigate', { ...event, url: origin + '/' });
  assert.equal(refused, 1, 'fixed-origin root navigation was refused');
  window.webContents.emit('will-frame-navigate', { ...event, url: 'https://example.com/' });
  window.webContents.emit('will-redirect', event);
  window.webContents.emit('will-attach-webview', event);
  session.emit('will-download', event);
  assert.equal(refused, 5);
});
