// This sandboxed preload exposes no ipcRenderer, Node, path, URL, filesystem or
// shell API. All identifiers and channel names stay in the isolated world.
const { contextBridge, ipcRenderer } = require('electron');
const connections = new Map();
let next = 0;

ipcRenderer.on('hataori:native:event', (_event, id, sequence, data) => {
  const receive = connections.get(id);
  if (!receive) return;
  try { receive(data); }
  finally {
    ipcRenderer.invoke('hataori:native:ack', id, sequence).catch(() => {});
    if (data.type === 'close') connections.delete(id);
  }
});

contextBridge.exposeInMainWorld('hataoriNative', {
  connect(path, receive) {
    if (!['/api/attach', '/api/operate'].includes(path) || typeof receive !== 'function' || connections.size >= 16) throw new Error('Refused native operation');
    const id = ++next;
    connections.set(id, receive);
    const opened = ipcRenderer.invoke('hataori:native:open', id, path);
    opened.catch(() => {
      if (connections.delete(id)) receive({ type: 'close', code: 1006, reason: '' });
    });
    return {
      async send(text) {
        if (typeof text !== 'string' || text.length > 65536 || !connections.has(id)) throw new Error('Refused native write');
        await opened;
        return ipcRenderer.invoke('hataori:native:write', id, text);
      },
      close() {
        connections.delete(id);
        opened.then(() => ipcRenderer.invoke('hataori:native:close', id)).catch(() => {});
      },
    };
  },
});
