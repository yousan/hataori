import assert from 'node:assert/strict';
import { test } from 'node:test';
import { origin, route, senderOK, preferences } from '../src/policy.mjs';

test('native routes cannot become network, control, credential or filesystem capabilities', () => {
  for (const raw of [
    'http://instance/api/me', 'https://example.com/', 'file:///etc/passwd',
    origin + '.evil/api/me', 'hataori-desktop://owner@instance/api/me',
    origin + '/api/../signin', origin + '/api/%2e%2e/send', origin + '/api//me',
    origin + '/api/me#token', origin + '/signin', origin + '/api/passkeys',
    origin + '/api/unknown', origin + '/assets/../secret', origin + '/assets/a\\b',
  ]) assert.throws(() => route(raw), raw);
  assert.throws(() => route(origin + '/api/send', 'GET'));
  assert.throws(() => route(origin + '/api/me', 'POST'));
  assert.throws(() => route(origin + '/api/me', 'DELETE'));
  assert.throws(() => route(origin + '/api/me', 'GET', true));
  assert.throws(() => route(origin + '/api/attach?root=other', 'GET', true));
  assert.equal(route(origin + '/api/events?after=21'), '/api/events?after=21');
  assert.equal(route(origin + '/api/send', 'POST'), '/api/send');
  assert.equal(route(origin + '/api/attach', 'GET', true), '/api/attach');
});

test('only the selected window main frame can own IPC streams', () => {
  const mainFrame = { url: origin + '/' };
  const contents = { mainFrame };
  assert.equal(senderOK({ sender: contents, senderFrame: mainFrame }, contents), true);
  assert.equal(senderOK({ sender: {}, senderFrame: mainFrame }, contents), false);
  assert.equal(senderOK({ sender: contents, senderFrame: { ...mainFrame } }, contents), false);
  for (const url of ['https://instance/', 'hataori-desktop://foreign/', 'about:blank', 'file:///']) {
    mainFrame.url = url;
    assert.equal(senderOK({ sender: contents, senderFrame: mainFrame }, contents), false);
  }
  assert.equal(preferences.sandbox, true);
  assert.equal(preferences.contextIsolation, true);
  assert.equal(preferences.nodeIntegration, false);
  assert.equal(preferences.webSecurity, true);
});
