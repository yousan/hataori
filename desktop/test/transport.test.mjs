import assert from 'node:assert/strict';
import { test } from 'node:test';
import { Duplex, PassThrough } from 'node:stream';
import http from 'node:http';
import { WebSocketServer } from 'ws';
import { NativeTransport } from '../src/transport.mjs';
import { origin } from '../src/policy.mjs';

function fixture(t, handler) {
  const server = http.createServer(handler);
  const connections = new Set();
  let calls = 0;
  const connect = () => {
    calls++;
    const a = new PassThrough(), b = new PassThrough();
    const client = Duplex.from({ readable: a, writable: b });
    const peer = Duplex.from({ readable: b, writable: a });
    connections.add(client);
    connections.add(peer);
    server.emit('connection', peer);
    return client;
  };
  const transport = new NativeTransport('', {}, connect);
  t.after(() => { transport.close(); for (const c of connections) c.destroy(); server.close(); });
  return { server, transport, calls: () => calls };
}

test('private fetch preserves CSP, streams and body while stripping browser authority', async t => {
  const csp = "default-src 'self'; object-src 'none'";
  let seen;
  const { transport, calls } = fixture(t, (req, res) => {
    seen = req;
    assert.equal(req.headers.host, 'instance');
    assert.equal(req.headers.origin, origin);
    assert.equal(req.headers.authorization, undefined);
    assert.equal(req.headers.cookie, undefined);
    assert.equal(req.headers.forwarded, undefined);
    res.setHeader('Content-Security-Policy', csp);
    res.setHeader('Content-Type', 'text/event-stream');
    res.write('id: 21\ndata: first\n\n');
    setImmediate(() => res.end('id: 22\ndata: next\n\n'));
  });
  const response = await transport.fetch(new Request(origin + '/api/events?after=20', {
    headers: { Authorization: 'browser-token', Cookie: 'browser-cookie', Forwarded: 'for=other', 'Last-Event-ID': '20' },
  }));
  assert.equal(response.headers.get('content-security-policy'), csp);
  assert.equal(await response.text(), 'id: 21\ndata: first\n\nid: 22\ndata: next\n\n');
  assert.equal(seen.headers['last-event-id'], '20');
  assert.equal(calls(), 1); // in-memory connection, never DNS/TCP
  await assert.rejects(transport.fetch(new Request(origin + '/api/me', { headers: { Origin: 'https://foreign.example' } })));
  assert.equal(calls(), 1);
});

test('duplex uses private connection, preserves message order and never replays after loss', async t => {
  const { server, transport, calls } = fixture(t, (_req, res) => res.end());
  const wss = new WebSocketServer({ noServer: true, perMessageDeflate: false });
  t.after(() => wss.close());
  server.on('upgrade', (req, socket, head) => {
    assert.equal(req.headers.host, 'instance');
    assert.equal(req.headers.origin, origin);
    assert.equal(req.url, '/api/attach');
    wss.handleUpgrade(req, socket, head, ws => {
      ws.on('message', data => ws.send(data.toString()));
    });
  });
  const received = [];
  let socket;
  await new Promise((resolve, reject) => {
    socket = transport.socket('/api/attach', event => {
      if (event.type === 'open') socket.send('{"session":"","thread":1}').catch(reject);
      if (event.type === 'error') reject(new Error('duplex failed'));
      if (event.type === 'message') { received.push(event.data); resolve(); }
    });
  });
  assert.deepEqual(received, ['{"session":"","thread":1}']);
  assert.equal(calls(), 1);
  transport.close();
  await assert.rejects(socket.send('never replay'));
  assert.throws(() => transport.socket('/api/attach', () => {}));
  assert.equal(calls(), 1);
});

test('closing transport cancels a held request body before any connection or owner write', async () => {
  let connections = 0, canceled = 0;
  const transport = new NativeTransport('', {}, () => { connections++; throw new Error('must not connect'); });
  const body = new ReadableStream({ cancel() { canceled++; } });
  const request = new Request(origin + '/api/send', { method: 'POST', body, duplex: 'half' });
  const pending = transport.fetch(request);
  transport.close();
  await assert.rejects(pending, /Private request ended/u);
  assert.equal(canceled, 1);
  assert.equal(connections, 0);
});
