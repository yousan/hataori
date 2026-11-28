import assert from 'node:assert/strict';
import { test } from 'node:test';
import { EventEmitter } from 'node:events';
import { PassThrough } from 'node:stream';
import { checkedBinding, checkedRegistration, guiEnvironment, startSetup, startManaged, enableLogin, disableLogin } from '../src/lifecycle.mjs';

test('setup lifetime is private, bounded and joined before ordinary startup', async () => {
  let args, options;
  const child = Object.assign(new EventEmitter(), { stdin: new PassThrough(), stdout: new PassThrough() });
  const setup = startSetup('/fixture/hataori', '/fixture/root', '1.0.0', (_binary, a, o) => { args = a; options = o; return child; });
  assert.deepEqual(args, ['desktop', 'setup', '--root', '/fixture/root']);
  assert.equal(options.shell, false); assert.deepEqual(options.stdio, ['pipe', 'pipe', 'ignore']);
  const binding = { Root: '/fixture/root', Instance: 'fixture-installation', Version: '1.0.0' };
  child.stdout.write(JSON.stringify({ event: 'setup', binding }) + '\n');
  assert.deepEqual(await setup.ready, binding);
  let joined = false; setup.done.then(() => { joined = true; });
  child.stdout.write(JSON.stringify({ event: 'configured', root: binding.Root, instance: binding.Instance }) + '\n');
  await Promise.resolve(); assert.equal(joined, false);
  child.emit('close', 0); assert.deepEqual(await setup.done, binding);
  assert.deepEqual(guiEnvironment({ HOME: '/home/fixture', PATH: '/bin', BOT_TOKEN: 'private', NODE_OPTIONS: '--require bad' }), { HOME: '/home/fixture', PATH: '/bin' });
  assert.throws(() => checkedBinding({ ...binding, Version: '0.9.0' }, '1.0.0'));
});

test('interrupted setup can explicitly recover registration and retry bootstrap without implicitly undoing Stop', async () => {
  let stopped = true, registered = false, consent = false, bootstrapFails = true;
  const calls = [];
  const actions = {
    registration: async () => { calls.push('read'); if (!registered) throw new Error('interrupted setup'); return 'own-registration'; },
    register: async () => { calls.push('consent'); if (!consent) throw new Error('cancel'); registered = true; return 'own-registration'; },
    persistStart: async () => { calls.push('start'); stopped = false; },
    enable: async reg => { assert.equal(reg, 'own-registration'); calls.push('enable'); if (bootstrapFails) throw new Error('fixture bootstrap refusal'); },
    ready: async () => { calls.push('ready'); return 'bound-instance'; },
  };
  await assert.rejects(startManaged(actions)); assert.equal(stopped, true);
  consent = true; calls.length = 0;
  await assert.rejects(startManaged(actions)); assert.deepEqual(calls, ['read', 'consent', 'start', 'enable']);
  bootstrapFails = false; calls.length = 0;
  assert.equal(await startManaged(actions), 'bound-instance');
  assert.deepEqual(calls, ['read', 'start', 'enable', 'ready']);
});

test('login actions name only a verified exact service and preserve daemon ownership', async () => {
  const label = 'io.hataori.desktop.' + 'a'.repeat(32), directory = '/fixture/LaunchAgents';
  const reg = checkedRegistration({ label, path: `${directory}/${label}.plist`, root: '/fixture/root' }, '/fixture/root', directory);
  assert.throws(() => checkedRegistration({ ...reg, path: '/foreign/job.plist' }, reg.root, directory));
  const calls = [];
  const run = async (binary, args) => { assert.equal(binary, '/bin/launchctl'); calls.push(args); if (args[0] === 'print') throw new Error('not loaded'); };
  await enableLogin(reg, 501, run);
  assert.deepEqual(calls.map(a => a[0]), ['enable', 'print', 'bootstrap', 'kickstart']);
  assert.equal(calls.flat().includes('-k'), false);
  calls.length = 0; await disableLogin(reg, 501, run);
  assert.deepEqual(calls, [['disable', `gui/501/${label}`], ['bootout', `gui/501/${label}`]]);
});
