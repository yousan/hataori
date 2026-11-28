// Every renderer capability is bound to one main frame and one private origin.
export const origin = 'hataori-desktop://instance';
export const scheme = 'hataori-desktop';
export const maxBody = 1024 * 1024;
export const maxMessage = 64 * 1024;

const reads = new Set([
  '/api/native', '/api/threads', '/api/scopes', '/api/layout', '/api/thread',
  '/api/threadevents', '/api/events', '/api/terminal', '/api/hostscreen',
  '/api/limits', '/api/clients', '/api/trusts', '/api/workspace',
  '/api/hostterminals', '/api/me', '/api/act/may', '/api/operate/may',
]);
const writes = new Set([
  '/api/act/check', '/api/send', '/api/answer', '/api/stop', '/api/start/choose',
  '/api/operate/check',
]);
export const duplex = new Set(['/api/operate', '/api/attach']);

export function route(raw, method = 'GET', socket = false, setup = false) {
  if (typeof raw !== 'string' || raw.length > 8192 || /[\\\x00-\x20\x7f]/u.test(raw)) throw new Error('Refused route');
  // Validate before URL normalization can erase dot segments or credentials.
  if (!raw.startsWith(origin + '/') || raw.includes('#')) throw new Error('Refused origin');
  const target = raw.slice(origin.length);
  const pathname = target.split('?')[0];
  if (pathname.includes('%') || pathname.includes('//') || pathname.split('/').some(p => p === '.' || p === '..')) throw new Error('Refused path');
  const url = new URL(raw);
  if (url.protocol !== scheme + ':' || url.host !== 'instance' || url.username || url.password) throw new Error('Refused origin');
  if (setup) {
    if (socket || !((method === 'GET' || method === 'HEAD') && (pathname === '/' || pathname === '/api/native' || pathname === '/api/setup' || /^\/assets\/[A-Za-z0-9_.-]+$/u.test(pathname))) && !(method === 'POST' && pathname === '/api/setup')) throw new Error('Refused setup route');
    return target;
  }
  if (socket) {
    if (method !== 'GET' || url.search || !duplex.has(pathname)) throw new Error('Refused duplex route');
  } else if (!((method === 'GET' || method === 'HEAD') &&
    (reads.has(pathname) || pathname === '/' || /^\/assets\/[A-Za-z0-9_.-]+$/u.test(pathname))) &&
    !(method === 'POST' && writes.has(pathname))) throw new Error('Refused method or route');
  return target;
}

export function senderOK(event, contents) {
  const frame = event.senderFrame;
  if (event.sender !== contents || !frame || frame !== contents.mainFrame) return false;
  try {
    const url = new URL(frame.url);
    return url.protocol === scheme + ':' && url.host === 'instance' && url.pathname === '/' && !url.username && !url.password;
  } catch { return false; }
}

export const preferences = Object.freeze({
  nodeIntegration: false, nodeIntegrationInWorker: false, nodeIntegrationInSubFrames: false,
  contextIsolation: true, sandbox: true, webSecurity: true, webviewTag: false,
  allowRunningInsecureContent: false, spellcheck: false,
});
