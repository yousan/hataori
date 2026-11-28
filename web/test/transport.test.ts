import assert from "node:assert/strict";
import { test } from "node:test";
import { dashboardSocket } from "../src/transport.ts";

function globals(t: import("node:test").TestContext, values: Record<string, unknown>) {
  for (const [name, value] of Object.entries(values)) {
    const old = Object.getOwnPropertyDescriptor(globalThis, name);
    Object.defineProperty(globalThis, name, { configurable: true, writable: true, value });
    t.after(() => old ? Object.defineProperty(globalThis, name, old) : Reflect.deleteProperty(globalThis, name));
  }
}

test("browser duplex retains its original websocket and native never falls back to TCP", t => {
  const urls: string[] = [];
  class BrowserSocket { constructor(url: string) { urls.push(url); } }
  const location = { origin: "https://dashboard.example", protocol: "https:", host: "dashboard.example" };
  globals(t, { location, WebSocket: BrowserSocket, window: {} });
  assert.ok(dashboardSocket("/api/attach") instanceof BrowserSocket);
  assert.deepEqual(urls, ["wss://dashboard.example/api/attach"]);
  location.origin = "hataori-desktop://instance";
  assert.throws(() => dashboardSocket("/api/attach"));
  assert.throws(() => dashboardSocket("/api/unknown"));
  assert.equal(urls.length, 1);
});

test("native duplex orders acknowledged writes and discards pending input after loss", async t => {
  let receive: (event: { type: "open" | "close"; code?: number }) => void = () => {};
  let release: () => void = () => {};
  const sent: string[] = [];
  let closes = 0;
  globals(t, {
    location: { origin: "hataori-desktop://instance" },
    CloseEvent: class extends Event { code: number; reason: string; constructor(type: string, e: CloseEventInit) { super(type); this.code = e.code ?? 1006; this.reason = e.reason ?? ""; } },
    window: { hataoriNative: { connect(path: string, callback: typeof receive) {
      assert.equal(path, "/api/attach"); receive = callback;
      return {
        async send(text: string) { sent.push(text); if (sent.length === 1) await new Promise<void>(r => { release = r; }); },
        close() { closes++; },
      };
    } } },
  });
  const socket = dashboardSocket("/api/attach");
  let opens = 0, ends = 0;
  socket.onopen = () => { opens++; };
  socket.onclose = () => { ends++; };
  receive({ type: "open" });
  assert.equal(opens, 1);
  socket.send('{"session":"","thread":1}');
  socket.send('{"keys":"uncertain input"}');
  await Promise.resolve();
  assert.deepEqual(sent, ['{"session":"","thread":1}']);
  receive({ type: "close", code: 1006 });
  release();
  await new Promise(r => setImmediate(r));
  assert.equal(socket.readyState, 3);
  assert.equal(socket.bufferedAmount, 0);
  assert.equal(ends, 1);
  assert.equal(closes, 0);
  assert.equal(sent.length, 1);
  assert.throws(() => socket.send("never replay"));
});
