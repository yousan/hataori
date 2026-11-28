// Hosted-only Electron proof. Every daemon, session and repository belongs to
// the scratch fixture. This never starts the product LaunchAgent or an agent
// provider, changes login settings, looks up PIDs or uses a live instance.
import * as electron from 'electron';
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { mkdtemp } from 'node:fs/promises';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { NativeTransport } from '../src/transport.mjs';
import { createWindow } from '../src/window.mjs';
import { guiEnvironment } from '../src/lifecycle.mjs';

if (process.platform !== 'darwin' || process.env.GITHUB_ACTIONS !== 'true' || process.env.RUNNER_ENVIRONMENT !== 'github-hosted' || process.env.CI !== 'true') throw new Error('Disposable standard macOS runner required');
const { app, protocol } = electron;
app.enableSandbox();
protocol.registerSchemesAsPrivileged([{ scheme: 'hataori-desktop', privileges: { standard: true, secure: true, supportFetchAPI: true, stream: true, corsEnabled: true } }]);
const scratch = await mkdtemp(join(process.env.RUNNER_TEMP, 'hataori-electron-'));
app.setPath('userData', scratch); app.setPath('sessionData', scratch);
let child, window, transport, childDone;
const pending = new Map();
let childLog = '';
function awaitEvent(name) {
  let timer;
  return new Promise((resolve, reject) => {
    timer = setTimeout(() => { pending.delete(name); reject(new Error('Fixture event timed out: ' + name)); }, 60000);
    pending.set(name, { resolve(value) { clearTimeout(timer); resolve(value); }, reject(error) { clearTimeout(timer); reject(error); } });
  });
}
async function tell(command, expected) { const next = awaitEvent(expected); child.stdin.write(command + '\n'); return next; }
const proof = name => console.log('PASS desktop hosted ' + name);
async function run() {
  const ready = awaitEvent('ready');
  child = spawn(process.env.DESKTOP_FIXTURE_BINARY, ['-test.run=^TestDesktopHostedFixture$', '-test.v', '-test.parallel=1', '-test.timeout=3m'], {
    stdio: ['pipe', 'pipe', 'pipe'], shell: false,
    env: { ...guiEnvironment(), GITHUB_ACTIONS: 'true', RUNNER_ENVIRONMENT: 'github-hosted', HATAORI_DESKTOP_HOSTED_FIXTURE: '1' },
  });
  let buffer = '';
  child.stdout.setEncoding('utf8');
  child.stdout.on('data', chunk => {
    childLog = (childLog + chunk).slice(-16384); buffer += chunk;
    if (buffer.length > 32768) { for (const p of pending.values()) p.reject(new Error('Fixture output limit')); pending.clear(); return; }
    let at;
    while ((at = buffer.indexOf('\n')) !== -1) {
      const line = buffer.slice(0, at); buffer = buffer.slice(at + 1);
      if (!line.startsWith('DESKTOP_FIXTURE ')) continue;
      try { const event = JSON.parse(line.slice(16)); pending.get(event.event)?.resolve(event); pending.delete(event.event); }
      catch { for (const p of pending.values()) p.reject(new Error('Invalid private fixture event')); pending.clear(); }
    }
  });
  child.stderr.on('data', chunk => { childLog = (childLog + chunk).slice(-16384); });
  childDone = new Promise((resolve, reject) => {
    const fail = error => { for (const p of pending.values()) p.reject(error); pending.clear(); reject(error); };
    child.once('error', () => fail(new Error('Private fixture could not start')));
    child.once('close', code => code === 0 ? resolve() : fail(new Error('Private fixture failed')));
  });
  childDone.catch(() => {});
  const fixture = await ready;
  assert.equal(fixture.binding.Version, process.env.DESKTOP_PROOF_VERSION);
  transport = new NativeTransport(process.env.DESKTOP_TRANSPORT_BINARY, fixture.binding);
  const original = await transport.fetch(new Request('hataori-desktop://instance/'));
  const csp = original.headers.get('content-security-policy');
  assert.ok(csp?.includes("default-src 'self'")); assert.ok(!csp.includes('unsafe-eval'));
  assert.match(await original.text(), /\/assets\/index-[\w-]+\.js/u);
  await app.whenReady();
  window = await createWindow(electron, transport, join(dirname(fileURLToPath(import.meta.url)), '../src/preload.cjs'));
  const evaluate = code => window.webContents.executeJavaScript(code);
  const security = await evaluate(`({ origin:location.origin, require:typeof require, process:typeof process, buffer:typeof Buffer, bridge:Object.keys(window.hataoriNative) })`);
  assert.equal(security.origin, 'hataori-desktop://instance');
  for (const name of ['require', 'process', 'buffer']) assert.equal(security[name], 'undefined');
  assert.deepEqual(security.bridge, ['connect']);
  const prefs = window.webContents.getLastWebPreferences();
  assert.equal(prefs.nodeIntegration, false); assert.equal(prefs.contextIsolation, true); assert.equal(prefs.sandbox, true); assert.equal(prefs.webSecurity, true);
  proof('existing-page-csp-and-sandbox');
  const owner = await evaluate(`fetch('/api/me').then(async r=>({status:r.status,body:await r.json()}))`);
  assert.equal(owner.status, 200); assert.equal(owner.body.way, 'native');
  const threads = await evaluate(`fetch('/api/threads').then(r=>r.json())`);
  assert.ok(threads.some(t => t.id === fixture.thread));
  for (const target of ['https://example.invalid/', 'http://localhost/api/me', 'file:///etc/passwd', '/api/session']) {
    const refused = await evaluate(`fetch(${JSON.stringify(target)}).then(()=>false,()=>true)`); assert.equal(refused, true);
  }
  assert.equal(await evaluate(`window.open('https://example.invalid/') === null`), true);
  await evaluate(`{ const a=document.createElement('a'); a.href='https://example.invalid/'; a.click(); }`);
  await new Promise(resolve => setTimeout(resolve, 100));
  assert.equal(window.webContents.getURL(), 'hataori-desktop://instance/');
  assert.equal(await evaluate(`(()=>{try{window.hataoriNative.connect('/api/session',()=>{});return false;}catch{return true;}})()`), true);
  proof('remote-navigation-and-unexpected-capability-refused');
  // Read one bounded SSE record using Chromium fetch, then cancel its producer.
  const first = async (path, after = '') => evaluate(`(async()=>{
    const response=await fetch(${JSON.stringify(path)},{headers:${JSON.stringify(after ? { 'Last-Event-ID': after } : {})}});
    if(!response.ok) throw Error('stream refused'); const reader=response.body.getReader(); let text='';
    try { while(!text.includes('\\n\\n')) { const part=await reader.read(); if(part.done) break; text+=new TextDecoder().decode(part.value); if(text.length>1048576) throw Error('stream limit'); } return text; }
    finally { await reader.cancel(); }
  })()`);
  const events = await first('/api/events'); assert.match(events, /(?:id:|event: caught_up)/u);
  const terminal = await first('/api/terminal?thread=' + fixture.thread);
  const position = /id: ([0-9]+):([^\s]+)/u.exec(terminal); assert.ok(position, 'terminal stream position');
  proof('fetch-event-and-terminal-streams');
  const attach = async session => evaluate(`new Promise(async(resolve,reject)=>{
    let channel; const timer=setTimeout(()=>reject(Error('attach timeout')),15000);
    try { channel=await window.hataoriNative.connect('/api/attach',event=>{
      if(event.type==='open') channel.send(JSON.stringify({thread:${fixture.thread},session:${JSON.stringify(session)}})).catch(reject);
      if(event.type==='message') { const data=JSON.parse(event.data); if(data.attached) channel.send(JSON.stringify({end:true})).catch(reject); if(data.ended||data.refused){clearTimeout(timer);channel.close();resolve(data);} }
      if(event.type==='error'){clearTimeout(timer);reject(Error('attach failed'));}
    }); } catch(error) {clearTimeout(timer);reject(error);}
  })`);
  assert.equal((await attach('browser-token')).refused, 'not signed in');
  assert.equal((await attach('')).ended, 'ended'); proof('private-duplex-first-message-and-close');
  await tell('turn', 'turn'); await tell('restart', 'restarted');
  const after = await first('/api/terminal?thread=' + fixture.thread, position[1] + ':' + position[2]);
  const resumed = /id: ([0-9]+):([^\s]+)/u.exec(after); assert.ok(resumed); assert.equal(resumed[2], position[2]); assert.ok(Number(resumed[1]) >= Number(position[1]));
  await tell('finish', 'finished');
  assert.equal((await attach('')).ended, 'ended');
  proof('daemon-only-held-fake-turn-host-and-offset-reconnect');
  transport.close(); window.destroy(); window = null;
  child.stdin.end(); await childDone;
  assert.match(childLog, /--- PASS: TestDesktopHostedFixture /u);
  assert.ok(!childLog.includes('--- SKIP:'));
  proof('fixture-graceful-cleanup-no-pid-lookup');
}
try { await run(); app.exit(0); }
catch (error) {
  transport?.close(); window?.destroy(); child?.stdin.end();
  if (childDone) await childDone.catch(() => {});
  console.error('Desktop hosted proof failed:', error.message);
  // The fixture uses only synthetic data; emit bounded diagnostic context.
  if (childLog) console.error(childLog);
  app.exit(1);
}
