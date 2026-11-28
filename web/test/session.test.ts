// Isolated session bootstrap tests: no browser, socket or external request.

import { test, type TestContext } from "node:test";
import assert from "node:assert/strict";

let fixture = 0;

async function page(t: TestContext, origin: string, options: {
  link?: string;
  stored?: string;
  storageThrows?: boolean;
  answer?: { status: number; session?: string };
} = {}) {
  const calls = { storage: 0, saved: [] as string[], replaced: [] as unknown[][], fetch: [] as { path: string; init: RequestInit }[] };
  const location = { origin, pathname: options.link ? "/signin" : "/", hash: options.link ? `#t=${options.link}` : "" };
  const globals = {
    location,
    history: { replaceState: (...args: unknown[]) => calls.replaced.push(args) },
    localStorage: {
      getItem(key: string) {
        calls.storage++;
        assert.equal(key, "hataori-session");
        if (options.storageThrows) throw new Error("storage unavailable");
        return options.stored ?? null;
      },
      setItem(key: string, value: string) {
        assert.equal(key, "hataori-session");
        calls.saved.push(value);
      },
    },
    fetch: async (path: string, init: RequestInit) => {
      calls.fetch.push({ path, init });
      if (!options.answer) throw new Error("unexpected sign-in request");
      return new Response(JSON.stringify({ session: options.answer.session }), { status: options.answer.status });
    },
  };
  for (const [key, value] of Object.entries(globals)) {
    const previous = Object.getOwnPropertyDescriptor(globalThis, key);
    Object.defineProperty(globalThis, key, { configurable: true, writable: true, value });
    t.after(() => {
      if (previous) Object.defineProperty(globalThis, key, previous);
      else Reflect.deleteProperty(globalThis, key);
    });
  }
  // Every case gets fresh module state, including the cached session/proof.
  const session = await import(`../src/session.ts?fixture=${++fixture}`) as typeof import("../src/session.ts");
  return { session, calls, location };
}

test("native bootstrap ignores browser link and stored credentials", async (t) => {
  const { session, calls } = await page(t, "hataori-desktop://instance", {
    link: "fixture-link", stored: "browser-session", answer: { status: 200, session: "link-session" },
  });
  await session.adoptSession();
  assert.equal(session.sessionToken(), null);
  assert.equal(session.registerProof(), null);
  assert.equal(session.linkRefused(), false);
  assert.equal(new Headers(session.authorized().headers).has("Authorization"), false);
  assert.deepEqual(calls, { storage: 0, saved: [], replaced: [], fetch: [] });
});

test("native bootstrap does not access unavailable browser storage", async (t) => {
  const { session, calls } = await page(t, "hataori-desktop://instance", { storageThrows: true });
  await session.adoptSession();
  assert.equal(session.sessionToken(), null);
  assert.equal(calls.storage, 0);
});

test("native bootstrap clears a previously cached browser session", async (t) => {
  const { session, location, calls } = await page(t, "https://dashboard.example", { stored: "browser-session" });
  await session.adoptSession();
  assert.equal(session.sessionToken(), "browser-session");
  location.origin = "hataori-desktop://instance";
  await session.adoptSession();
  assert.equal(session.sessionToken(), null);
  assert.equal(new Headers(session.authorized().headers).has("Authorization"), false);
  assert.equal(calls.storage, 1);
});

for (const origin of [
  "http://localhost:8080", "https://dashboard.example", "http://instance",
  "hataori-desktop://other", "hataori-desktop://instance.example", "hataori-desktop://instance:8080", "null",
]) {
  test(`non-native origin retains browser storage bootstrap: ${origin}`, async (t) => {
    const { session, calls } = await page(t, origin, { stored: "browser-session" });
    await session.adoptSession();
    assert.equal(session.sessionToken(), "browser-session");
    assert.equal(new Headers(session.authorized().headers).get("Authorization"), "Bearer browser-session");
    assert.equal(calls.storage, 1);
    assert.equal(calls.fetch.length, 0);
  });
}

test("browser sign-in still exchanges the link before using stored credentials", async (t) => {
  const { session, calls } = await page(t, "https://dashboard.example", {
    link: "fixture-link", stored: "old-session", answer: { status: 200, session: "link-session" },
  });
  await session.adoptSession();
  assert.equal(session.sessionToken(), "link-session");
  assert.deepEqual(calls.fetch, [{ path: "/api/session", init: {
    headers: { Authorization: "Link fixture-link" }, credentials: "omit", cache: "no-store",
  } }]);
  assert.deepEqual(calls.replaced, [[null, "", "/"]]);
  assert.deepEqual(calls.saved, ["link-session"]);
  assert.equal(calls.storage, 0);
});

test("a refused browser link retains the previous session and refusal state", async (t) => {
  const { session } = await page(t, "https://dashboard.example", {
    link: "fixture-link", stored: "browser-session", answer: { status: 401 },
  });
  await session.adoptSession();
  assert.equal(session.sessionToken(), "browser-session");
  assert.equal(session.linkRefused(), true);
});

test("browser storage failure leaves the page signed out", async (t) => {
  const { session, calls } = await page(t, "https://dashboard.example", { storageThrows: true });
  await session.adoptSession();
  assert.equal(session.sessionToken(), null);
  assert.equal(calls.storage, 1);
});
