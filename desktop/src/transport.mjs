import { spawn } from 'node:child_process';
import http from 'node:http';
import { Duplex, Readable } from 'node:stream';
import WebSocket from 'ws';
import { maxBody, maxMessage, origin, route } from './policy.mjs';

const requestHeaders = new Set(['accept', 'content-type', 'last-event-id', 'if-none-match', 'if-modified-since', 'range']);
const hopHeaders = new Set(['connection', 'keep-alive', 'transfer-encoding', 'upgrade', 'proxy-authenticate', 'proxy-authorization', 'te', 'trailer']);

// Each HTTP/duplex connection gets a fresh private child pipe. The matching
// bundled Go helper owns discovery/ACL/kernel checks on the actual connection.
// No HTTP authority travels through localhost, a public socket or a token.
export function openPipe(helper, binding, spawnChild = spawn) {
  const env = Object.fromEntries(['HOME', 'USER', 'LOGNAME', 'TMPDIR', 'LANG', 'LC_ALL']
    .filter(k => process.env[k] !== undefined).map(k => [k, process.env[k]]));
  const child = spawnChild(helper, ['--root', binding.Root, '--instance', binding.Instance], {
    stdio: ['pipe', 'pipe', 'ignore'], env, shell: false,
  });
  const pipe = Duplex.from({ readable: child.stdout, writable: child.stdin });
  const close = () => {
    // This handle is the exact child we created, never a stored PID/group.
    child.kill('SIGTERM');
  };
  pipe.once('close', close);
  child.once('error', () => pipe.destroy(new Error('Private transport unavailable')));
  child.once('exit', code => {
    if (code !== 0 && !pipe.destroyed) pipe.destroy(new Error('Private transport ended'));
  });
  return pipe;
}

export class NativeTransport {
  constructor(helper, binding, connect = () => openPipe(helper, binding)) {
    this.connect = connect;
    this.pending = new Set();
    this.closed = false;
  }

  async fetch(request) {
    if (this.closed) throw new Error('Transport closed');
    const target = route(request.url, request.method);
    const suppliedOrigin = request.headers.get('origin');
    if (suppliedOrigin && suppliedOrigin !== origin) throw new Error('Refused origin');
    // Chromium never selects the native identity or forwards browser tokens.
    const headers = { Host: 'instance', Origin: origin, Connection: 'close' };
    for (const [name, value] of request.headers) {
      if (requestHeaders.has(name.toLowerCase())) headers[name] = value;
    }
    const body = request.body ? Buffer.from(await limitedBody(request.body)) : null;
    if (body) headers['Content-Length'] = String(body.length);
    return new Promise((resolve, reject) => {
      const req = http.request({ host: 'instance', port: 80, path: target, method: request.method,
        headers, agent: null, createConnection: this.connect });
      this.pending.add(req);
      const abort = () => req.destroy(new Error('Private request ended'));
      request.signal.addEventListener('abort', abort, { once: true });
      const timer = setTimeout(abort, 10000); // response headers, not stream lifetime
      req.once('error', reject);
      req.once('close', () => {
        clearTimeout(timer);
        this.pending.delete(req);
        request.signal.removeEventListener('abort', abort);
      });
      req.once('response', response => {
        clearTimeout(timer);
        const headers = new Headers();
        for (let i = 0; i < response.rawHeaders.length; i += 2) {
          const name = response.rawHeaders[i];
          if (!hopHeaders.has(name.toLowerCase())) headers.append(name, response.rawHeaders[i + 1]);
        }
        // Response never follows redirects. Native daemon routes do not send
        // any; reject one rather than allowing remote content or credential URLs.
        if (response.statusCode >= 300 && response.statusCode < 400 && response.statusCode !== 304) {
          response.destroy();
          reject(new Error('Private redirect refused'));
          return;
        }
        const noBody = request.method === 'HEAD' || [204, 205, 304].includes(response.statusCode);
        resolve(new Response(noBody ? null : Readable.toWeb(response), { status: response.statusCode, headers }));
        if (noBody) response.resume();
      });
      if (request.signal.aborted) abort();
      else req.end(body);
    });
  }

  socket(path, onEvent) {
    if (this.closed) throw new Error('Transport closed');
    route(origin + path, 'GET', true);
    const ws = new WebSocket('ws://instance' + path, {
      origin, headers: { Host: 'instance' }, followRedirects: false,
      createConnection: this.connect, maxPayload: maxMessage,
      perMessageDeflate: false, handshakeTimeout: 10000,
    });
    this.pending.add(ws);
    ws.on('open', () => onEvent({ type: 'open' }));
    ws.on('message', (data, binary) => {
      if (binary) return ws.close(1003, 'Text required');
      onEvent({ type: 'message', data: data.toString('utf8') });
    });
    ws.on('close', (code, reason) => {
      this.pending.delete(ws);
      onEvent({ type: 'close', code, reason: reason.toString('utf8') });
    });
    ws.on('error', () => onEvent({ type: 'error' }));
    return {
      send: text => new Promise((resolve, reject) => {
        if (typeof text !== 'string' || Buffer.byteLength(text) > maxMessage || ws.bufferedAmount > maxMessage * 4 || ws.readyState !== WebSocket.OPEN) {
          reject(new Error('Duplex write refused')); return;
        }
        ws.send(text, err => err ? reject(new Error('Duplex write failed')) : resolve());
      }),
      close: () => ws.terminate(),
    };
  }

  close() {
    this.closed = true;
    for (const connection of this.pending) {
      if (typeof connection.terminate === 'function') connection.terminate();
      else connection.destroy();
    }
    this.pending.clear();
  }
}

async function limitedBody(stream) {
  const reader = stream.getReader();
  const chunks = [];
  let length = 0;
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) return Buffer.concat(chunks, length);
      length += value.byteLength;
      if (length > maxBody) throw new Error('Request body too large');
      chunks.push(Buffer.from(value));
    }
  } catch (error) {
    await reader.cancel().catch(() => {});
    throw error;
  } finally { reader.releaseLock(); }
}
