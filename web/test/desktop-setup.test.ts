import assert from "node:assert/strict";
import { test } from "node:test";
import { readDesktopSetup } from "../src/desktop-setup.ts";

test("ordinary native identity never asks for setup or adopts browser credentials", async () => {
  const calls: unknown[] = [];
  const request = (async (...args: unknown[]) => { calls.push(args); return Response.json({ native: true }); }) as typeof fetch;
  assert.equal(await readDesktopSetup(request), null);
  assert.deepEqual(calls, [["/api/native"]]);
});

test("restricted first-run metadata is read only after explicit setup identity and rejects malformed state", async () => {
  let setup = false;
  const info = { existing: true, agents: [] };
  const request = (async (url: unknown, options?: unknown) => {
    assert.equal(options, undefined);
    if (url === "/api/native") return Response.json({ setup: true });
    assert.equal(url, "/api/setup"); setup = true; return Response.json(info);
  }) as typeof fetch;
  assert.deepEqual(await readDesktopSetup(request), info); assert.equal(setup, true);
  await assert.rejects(readDesktopSetup((async () => new Response("", { status: 403 })) as typeof fetch));
  const malformed = (async (url: unknown) => Response.json(url === "/api/native" ? { setup: true } : { existing: false, agents: [{ name: null }] })) as typeof fetch;
  await assert.rejects(readDesktopSetup(malformed));
});
