import assert from 'node:assert/strict';
import { test } from 'node:test';
import { EventEmitter } from 'node:events';
import { installIPC } from '../src/ipc.mjs';
import { origin } from '../src/policy.mjs';

test('IPC rejects foreign frames, unexpected routes, unknown streams and unbounded queues', async () => {
  const handlers = new Map(), events = [];
  const ipc = { handle: (name, f) => handlers.set(name, f), removeHandler: name => handlers.delete(name) };
  const contents = Object.assign(new EventEmitter(), {
    mainFrame: { url: origin + '/' }, isDestroyed: () => false, send: (...args) => events.push(args),
  });
  const event = { sender: contents, senderFrame: contents.mainFrame };
  const callbacks = [];
  let closes = 0, writes = [];
  const dispose = installIPC(ipc, contents, { socket(path, callback) {
    callbacks.push(callback);
    return { send: async text => { writes.push(text); }, close: () => { closes++; } };
  } });
  const invoke = (op, e, ...args) => handlers.get('hataori:native:' + op)(e, ...args);
  assert.throws(() => invoke('open', { sender: {}, senderFrame: contents.mainFrame }, 1, '/api/attach'));
  assert.throws(() => invoke('open', { sender: contents, senderFrame: { url: origin + '/' } }, 1, '/api/attach'));
  assert.throws(() => invoke('open', event, 1, '/api/unknown'));
  invoke('open', event, 1, '/api/attach');
  assert.throws(() => invoke('open', event, 1, '/api/attach'));
  await invoke('write', event, 1, '{"keys":"once"}');
  assert.deepEqual(writes, ['{"keys":"once"}']);
  await assert.rejects(invoke('write', event, 99, 'foreign stream'));
  await assert.rejects(invoke('write', event, 1, 'x'.repeat(65537)));
  for (let i = 0; i < 5; i++) callbacks[0]({ type: 'message', data: 'x'.repeat(65536) });
  assert.equal(events.length, 5);
  assert.equal(events[4][3].code, 1009);
  assert.equal(closes, 1);
  await assert.rejects(invoke('write', event, 1, 'discarded'));
  invoke('open', event, 2, '/api/operate');
  contents.emit('did-start-navigation', {}, origin + '/', false, true);
  assert.equal(closes, 2);
  dispose();
  assert.equal(handlers.size, 0);
});
