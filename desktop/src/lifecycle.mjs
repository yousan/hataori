import { spawn, execFile } from 'node:child_process';
import { promisify } from 'node:util';
import path from 'node:path';

const execute = promisify(execFile);
export function guiEnvironment(source = process.env) {
  return Object.fromEntries(['HOME', 'USER', 'LOGNAME', 'PATH', 'TMPDIR', 'LANG', 'LC_ALL']
    .filter(key => source[key] !== undefined).map(key => [key, source[key]]));
}

export function checkedBinding(value, version) {
  if (!value || value.Version !== version || typeof value.Root !== 'string' || !path.isAbsolute(value.Root) || value.Root.includes('\0') || typeof value.Instance !== 'string' || value.Instance.length < 1 || value.Instance.length > 256) throw new Error('Invalid private instance binding');
  return Object.freeze({ Root: value.Root, Instance: value.Instance, Version: value.Version });
}

export async function command(binary, args, run = execute) {
  const { stdout } = await run(binary, args, { timeout: 30000, maxBuffer: 16384, env: guiEnvironment(), windowsHide: true, shell: false });
  return stdout ? JSON.parse(stdout) : null;
}

// Start is the only path that can clear persistent Stop. Missing registration
// goes through a fresh explicit-consent UI; a failed/cancelled recovery cannot
// start a daemon. A failed bootstrap can be retried through the same action.
export async function startManaged({ registration, register, persistStart, enable, ready }) {
  let reg;
  try { reg = await registration(); } catch { reg = await register(); }
  await persistStart();
  await enable(reg);
  return ready();
}

// Setup owns one child and one lifetime pipe. It never force-kills a process
// tree or looks up a saved PID. Success is usable only after child exit joins
// the restricted listener and releases the daemon's existing root lock.
export function startSetup(binary, root, version, spawnChild = spawn) {
  const child = spawnChild(binary, ['desktop', 'setup', '--root', root], { stdio: ['pipe', 'pipe', 'ignore'], env: guiEnvironment(), shell: false });
  let buffer = '', binding, configured = false, ended = false, failed = false;
  let readyResolve, readyReject, doneResolve, doneReject;
  const ready = new Promise((resolve, reject) => { readyResolve = resolve; readyReject = reject; });
  const done = new Promise((resolve, reject) => { doneResolve = resolve; doneReject = reject; });
  // A caller may still be awaiting ready when an early failure rejects done.
  done.catch(() => {});
  const fail = () => { failed = true; readyReject(new Error('Private setup unavailable')); child.stdin.end(); };
  const startup = setTimeout(fail, 15000);
  startup.unref();
  ready.then(() => clearTimeout(startup), () => clearTimeout(startup));
  child.stdout.setEncoding('utf8');
  child.stdout.on('data', chunk => {
    if (failed) return;
    buffer += chunk;
    if (Buffer.byteLength(buffer) > 16384) { fail(); return; }
    let newline;
    while ((newline = buffer.indexOf('\n')) !== -1) {
      const line = buffer.slice(0, newline); buffer = buffer.slice(newline + 1);
      try {
        const event = JSON.parse(line);
        if (!binding && event.event === 'setup') {
          binding = checkedBinding(event.binding, version); readyResolve(binding);
        } else if (binding && !configured && event.event === 'configured' && event.root === binding.Root && event.instance === binding.Instance) configured = true;
        else throw new Error('Unexpected setup event');
      } catch { fail(); }
    }
  });
  child.once('error', () => { fail(); ended = true; doneReject(new Error('Private setup unavailable')); });
  child.once('close', code => {
    ended = true;
    if (failed || code !== 0 || buffer.length || !binding || !configured) { readyReject(new Error('Private setup ended')); doneReject(new Error('Private setup ended')); }
    else doneResolve(binding);
  });
  return { ready, done, async close() {
    if (!ended) child.stdin.end();
    // A stuck child keeps its data/lock; failure never licenses a forced kill.
    let timer;
    try { await Promise.race([done.catch(() => {}), new Promise((_, reject) => { timer = setTimeout(() => reject(new Error('Private setup did not join')), 30000); })]); }
    finally { clearTimeout(timer); }
  } };
}

export function checkedRegistration(value, root, directory) {
  if (!value || value.root !== root || !/^io\.hataori\.desktop\.[0-9a-f]{32}$/u.test(value.label) || value.path !== path.join(directory, value.label + '.plist')) throw new Error('Invalid private login registration');
  return value;
}

export async function enableLogin(registration, uid, run = execute) {
  if (!Number.isSafeInteger(uid) || uid < 1) throw new Error('A GUI user is required');
  const domain = `gui/${uid}`, target = `${domain}/${registration.label}`;
  const options = { timeout: 15000, maxBuffer: 16384, env: guiEnvironment(), shell: false };
  await run('/bin/launchctl', ['enable', target], options);
  // print is an exact service lookup. Its output (which may contain private
  // paths or PIDs) is never parsed, logged, used as ownership or shown to UI.
  let loaded = false;
  try { await run('/bin/launchctl', ['print', target], options); loaded = true; } catch { /* bootstrap reports any real permission error */ }
  if (!loaded) await run('/bin/launchctl', ['bootstrap', domain, registration.path], options);
  await run('/bin/launchctl', ['kickstart', target], options); // no -k: never kill a loaded job
}

// The persistent stop is committed by the caller first. Disabling and
// booting out this exact proven registration leaves every data/binary file.
export async function disableLogin(registration, uid, run = execute) {
  if (!Number.isSafeInteger(uid) || uid < 1) throw new Error('A GUI user is required');
  const target = `gui/${uid}/${registration.label}`;
  const options = { timeout: 30000, maxBuffer: 16384, env: guiEnvironment(), shell: false };
  await run('/bin/launchctl', ['disable', target], options);
  await run('/bin/launchctl', ['bootout', target], options);
}
