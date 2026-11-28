import { duplex, maxMessage, senderOK } from './policy.mjs';

// A single window/instance owns these handlers. No channel accepts a root,
// binary, URL, identity or socket pathname from the renderer.
export function installIPC(ipc, contents, transport) {
  const streams = new Map();
  const names = [];
  const guard = event => {
    if (!senderOK(event, contents)) throw new Error('Refused IPC sender');
  };
  const handle = (name, f) => {
    names.push(name);
    ipc.handle(name, (event, ...args) => { guard(event); return f(event, ...args); });
  };
  const owned = (event, id) => {
    const stream = streams.get(id);
    if (!stream || stream.frame !== event.senderFrame) throw new Error('Unknown stream');
    return stream;
  };
  handle('hataori:native:open', (event, id, path) => {
    if (!Number.isSafeInteger(id) || id < 1 || streams.has(id) || streams.size >= 16 || !duplex.has(path)) throw new Error('Refused stream');
    const stream = { frame: event.senderFrame, socket: null, sequence: 0, unacked: new Map(), bytes: 0, writing: false };
    streams.set(id, stream);
    try {
      stream.socket = transport.socket(path, data => {
        if (!streams.has(id) || contents.isDestroyed()) return;
        const bytes = typeof data.data === 'string' ? Buffer.byteLength(data.data) : 0;
        stream.bytes += bytes;
        if (stream.bytes > maxMessage * 4 || stream.unacked.size >= 128) {
          streams.delete(id);
          contents.send('hataori:native:event', id, ++stream.sequence, { type: 'close', code: 1009, reason: 'Receive limit' });
          stream.socket?.close();
          return;
        }
        const sequence = ++stream.sequence;
        stream.unacked.set(sequence, bytes);
        contents.send('hataori:native:event', id, sequence, data);
        if (data.type === 'close') streams.delete(id);
      });
    } catch (error) { streams.delete(id); throw error; }
  });
  handle('hataori:native:write', async (event, id, text) => {
    const stream = owned(event, id);
    if (typeof text !== 'string' || Buffer.byteLength(text) > maxMessage || stream.writing) throw new Error('Refused duplex write');
    stream.writing = true;
    try { await stream.socket.send(text); }
    finally { stream.writing = false; }
  });
  handle('hataori:native:ack', (event, id, sequence) => {
    const stream = owned(event, id);
    if (!stream.unacked.has(sequence)) throw new Error('Unknown message');
    stream.bytes -= stream.unacked.get(sequence);
    stream.unacked.delete(sequence);
  });
  handle('hataori:native:close', (event, id) => {
    const stream = owned(event, id);
    streams.delete(id);
    stream.socket.close();
  });
  const drop = () => {
    for (const stream of streams.values()) stream.socket?.close();
    streams.clear();
  };
  contents.on('render-process-gone', drop);
  contents.on('did-start-navigation', (_event, _url, inPlace, mainFrame) => { if (mainFrame && !inPlace) drop(); });
  return () => { drop(); for (const name of names) ipc.removeHandler(name); };
}
