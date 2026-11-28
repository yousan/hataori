import * as electron from 'electron';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { NativeTransport } from './transport.mjs';
import { createWindow } from './window.mjs';
import { verifyManifest } from './manifest.mjs';

const { app, protocol, dialog, Menu, Tray, nativeImage } = electron;
protocol.registerSchemesAsPrivileged([{ scheme: 'hataori-desktop', privileges: {
  standard: true, secure: true, supportFetchAPI: true, stream: true, corsEnabled: true,
  // bypassCSP and service workers are deliberately absent.
} }]);
app.enableSandbox();
const run = promisify(execFile);
let window, tray, quitting = false, transport;
const show = () => { window?.show(); window?.focus(); };

// Coordination carries only "show this window"; argv and extra data from a
// second process can never change a selected root or perform owner operations.
if (!app.requestSingleInstanceLock()) app.quit();
else {
  app.on('second-instance', show);
  app.on('activate', show);
  app.on('before-quit', () => { quitting = true; transport?.close(); });
  app.whenReady().then(async () => {
    if (process.platform !== 'darwin' || !app.isPackaged) throw new Error('A packaged macOS build is required');
    const resources = process.resourcesPath;
    await verifyManifest(resources, app.getVersion());
    // The selected root is an explicit native file-picker action. No page,
    // external URL, command-line argument or second-instance payload selects it.
    const choice = await dialog.showOpenDialog({ title: 'Select a hataori instance', properties: ['openDirectory'] });
    if (choice.canceled || choice.filePaths.length !== 1) { app.quit(); return; }
    const helper = join(resources, 'hataori-desktop-transport');
    const { stdout } = await run(helper, ['--inspect', '--root', choice.filePaths[0]], {
      timeout: 15000, maxBuffer: 8192, env: {}, windowsHide: true,
    });
    const binding = JSON.parse(stdout);
    if (binding.Version !== app.getVersion() || typeof binding.Root !== 'string' || typeof binding.Instance !== 'string') throw new Error('Incompatible instance');
    transport = new NativeTransport(helper, binding);
    window = await createWindow(electron, transport, join(dirname(fileURLToPath(import.meta.url)), 'preload.cjs'));
    window.on('close', event => { if (!quitting) { event.preventDefault(); window.hide(); } });
    tray = new Tray(nativeImage.createEmpty());
    tray.setTitle('h');
    tray.setToolTip('hataori');
    tray.setContextMenu(Menu.buildFromTemplate([
      { label: 'Show hataori', click: show },
      { label: 'Updates are installed manually', enabled: false },
      { type: 'separator' },
      { label: 'Quit window (keep daemon running)', click: () => app.quit() },
    ]));
    tray.on('click', show);
    show();
  }).catch(() => {
    // Do not expose helper stderr, token-bearing URLs or private responses.
    dialog.showErrorBox('hataori', 'The selected private instance is unavailable or incompatible. Its data and daemon were left intact.');
    app.quit();
  });
}
