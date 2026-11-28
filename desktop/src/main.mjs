import * as electron from 'electron';
import { mkdir, realpath } from 'node:fs/promises';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { NativeTransport } from './transport.mjs';
import { createWindow } from './window.mjs';
import { verifyManifest } from './manifest.mjs';
import { command, checkedBinding, checkedRegistration, startSetup, enableLogin, disableLogin } from './lifecycle.mjs';

const { app, protocol, dialog, Menu, Tray, nativeImage, Notification } = electron;
protocol.registerSchemesAsPrivileged([{ scheme: 'hataori-desktop', privileges: {
  standard: true, secure: true, supportFetchAPI: true, stream: true, corsEnabled: true,
} }]);
app.enableSandbox();
let window, tray, transport, setup, binding, resources, binary, helper, root;
let quitting = false, managed = false, busy = false;
const show = () => { window?.show(); window?.focus(); };
const unavailable = () => dialog.showErrorBox('hataori', 'The selected private instance is unavailable or incompatible. Existing configuration, logins, data and session hosts were retained.');
const notify = body => {
  // Fixed main-process messages only; never terminal/event text or login URLs.
  if (Notification.isSupported()) new Notification({ title: 'hataori', body }).show();
};
const desktop = (operation, ...args) => command(binary, ['desktop', operation, '--root', root, ...args]);
const loginDirectory = async () => {
  const directory = join(app.getPath('home'), 'Library', 'LaunchAgents');
  await mkdir(directory, { recursive: true, mode: 0o700 });
  return realpath(directory);
};
const registration = async () => checkedRegistration(await desktop('login-info'), root, await loginDirectory());

// Stable facade across setup/daemon replacement. Reload drops old streams;
// no uncertain mutation or terminal input crosses a reconnect.
const facade = {
  get setup() { return transport?.setup === true; },
  fetch(request) { if (!transport) throw new Error('Private instance stopped'); return transport.fetch(request); },
  socket(path, receive) { if (!transport) throw new Error('Private instance stopped'); return transport.socket(path, receive); },
  close() { transport?.close(); },
};
async function display(next, setupMode = false) {
  transport?.close();
  binding = checkedBinding(next, app.getVersion()); root = binding.Root;
  transport = new NativeTransport(helper, binding, undefined, setupMode);
  if (window) await window.loadURL('hataori-desktop://instance/');
  else {
    window = await createWindow(electron, facade, join(dirname(fileURLToPath(import.meta.url)), 'preload.cjs'));
    window.on('close', event => { if (!quitting) { event.preventDefault(); window.hide(); } });
  }
  show();
}
async function inspect() {
  return checkedBinding(await command(helper, ['--inspect', '--root', root]), app.getVersion());
}
async function waitForDaemon() {
  const end = Date.now() + 30000;
  while (Date.now() < end && !quitting) {
    try {
      const next = await inspect();
      if (binding && next.Instance !== binding.Instance) throw new Error('Installation identity changed');
      return next;
    } catch {
      const state = await desktop('status');
      if (state.intent !== 'running') throw new Error('Persistent stop retained');
      await new Promise(resolve => setTimeout(resolve, 500));
    }
  }
  throw new Error('Daemon readiness not established');
}
async function stage() { return desktop('stage', '--resources', resources); }
async function start() {
  if (!managed) throw new Error('External instance is not app managed');
  const reg = await registration();
  await desktop('start'); await enableLogin(reg, process.getuid());
  await display(await waitForDaemon()); notify('Daemon started.');
}
async function stop() {
  if (!managed) throw new Error('External instance is not app managed');
  // Persist first; the launcher then closes only its exact child's pipe.
  await desktop('stop');
  transport?.close(); transport = null; window?.hide();
  notify('Daemon stop requested. Session hosts are retained.');
}
async function update() {
  if (!managed) throw new Error('External instance is not app managed');
  const current = await desktop('status');
  const choice = await dialog.showMessageBox({ type: 'question', title: 'hataori',
    message: `Use the installed app's daemon (${app.getVersion()}) instead of ${current.version}?`,
    detail: 'Only the daemon is replaced. Configuration, session hosts, old binaries and persistent Stop are retained. Incompatible schema or host protocols are refused.',
    buttons: ['Cancel', 'Use installed version'], defaultId: 0, cancelId: 0 });
  if (choice.response !== 1) return;
  const next = await stage();
  await desktop('select', '--version', next.version, '--binary', next.binary);
  if (current.intent === 'running') { transport?.close(); await display(await waitForDaemon()); }
  notify('Installed daemon version selected. Persistent Start/Stop was retained.');
}
async function uninstall() {
  if (!managed) throw new Error('External instance is not app managed');
  const choice = await dialog.showMessageBox({ type: 'warning', title: 'hataori',
    message: 'Disable this instance’s login launcher?', detail: 'Only this app’s exact registration is disabled. All instance data, workspaces, logins, detached hosts and referenced binaries remain. You can then remove the app manually.',
    buttons: ['Cancel', 'Disable launcher'], defaultId: 0, cancelId: 0 });
  if (choice.response !== 1) return;
  const reg = await registration(); await stop(); await disableLogin(reg, process.getuid()); app.quit();
}
function action(fn) {
  return async () => {
    if (busy || setup || quitting) return;
    busy = true; menu();
    try { await fn(); } catch { unavailable(); }
    finally { busy = false; menu(); }
  };
}
function menu() {
  tray?.setContextMenu(Menu.buildFromTemplate([
    { label: 'Show hataori', click: show, enabled: !!window },
    { label: 'Start daemon', enabled: managed && !busy && !setup, click: action(start) },
    { label: 'Stop daemon', enabled: managed && !busy && !setup, click: action(stop) },
    { label: 'Use installed app’s daemon version…', enabled: managed && !busy && !setup, click: action(update) },
    { label: 'Disable login launcher…', enabled: managed && !busy && !setup, click: action(uninstall) },
    { type: 'separator' },
    { label: 'Quit window (keep daemon running)', click: () => app.quit() },
  ]));
}
async function firstRun() {
  setup = startSetup(binary, root, app.getVersion()); menu();
  const session = setup;
  try {
    await display(await session.ready, true);
    await session.done; // restricted listener/handlers/root lease joined
    transport?.close(); transport = null;
    if (quitting) return;
    const selected = await stage();
    await desktop('register', '--version', selected.version, '--binary', selected.binary);
    const directory = await loginDirectory();
    const reg = checkedRegistration(await desktop('login-install', '--login-directory', directory), root, directory);
    managed = true;
    // The restricted page's initially unchecked checkbox authorized this root.
    await desktop('start'); await enableLogin(reg, process.getuid());
    await display(await waitForDaemon()); notify('Private instance configured and started.');
  } finally { await session.close(); setup = null; menu(); }
}
async function open() {
  const choice = await dialog.showMessageBox({ title: 'hataori', message: 'Choose a private instance',
    buttons: ['Cancel', 'Create default instance', 'Open existing instance'], defaultId: 2, cancelId: 0 });
  if (!choice.response) { app.quit(); return; }
  if (choice.response === 1) root = join(app.getPath('home'), 'Library', 'Application Support', 'hataori', 'instances', 'default');
  else {
    const picked = await dialog.showOpenDialog({ title: 'Select a hataori instance', properties: ['openDirectory'] });
    if (picked.canceled || picked.filePaths.length !== 1) { app.quit(); return; }
    root = picked.filePaths[0];
  }
  let state;
  try { state = await desktop('status'); managed = true; root = await realpath(root); } catch { /* no implicit adoption */ }
  menu();
  if (managed) {
    if (state.version !== app.getVersion()) await update();
    const current = await desktop('status');
    if (current.intent === 'stopped') {
      await dialog.showMessageBox({ title: 'hataori', message: 'Daemon remains stopped. Use Start daemon in the menu bar when ready.', buttons: ['OK'] });
      return;
    }
    await display(await waitForDaemon()); return;
  }
  try { await display(await inspect()); return; } catch { /* setup must still acquire the root lock */ }
  await firstRun();
}

// Coordination only shows this window; argv/extra data never select authority.
if (process.argv.includes('--version')) { console.log(app.getVersion()); app.exit(0); }
else if (!app.requestSingleInstanceLock()) app.quit();
else {
  app.on('second-instance', show); app.on('activate', show);
  app.on('before-quit', event => {
    if (quitting) return;
    quitting = true; transport?.close();
    if (setup) {
      event.preventDefault();
      setup.close().then(() => app.quit()).catch(() => { unavailable(); quitting = false; });
    }
  });
  app.whenReady().then(async () => {
    if (process.platform !== 'darwin' || !app.isPackaged) throw new Error('A packaged macOS build is required');
    resources = process.resourcesPath; await verifyManifest(resources, app.getVersion());
    binary = join(resources, 'hataori'); helper = join(resources, 'hataori-desktop-transport');
    tray = new Tray(nativeImage.createEmpty()); tray.setTitle('h'); tray.setToolTip('hataori');
    tray.on('click', show); menu(); await open();
  }).catch(() => { unavailable(); app.quit(); });
}
