// Unit tests of the tree as the chat shows it (src/chatTree.ts). Run with
// `node --test test/` (npm run check runs them).

import { test } from "node:test";
import assert from "node:assert/strict";
import type { ChatLayout, ThreadInfo } from "../src/api.ts";
import { chatTree, openInChat } from "../src/chatTree.ts";

const thread = (id: number, scope: string, state: ThreadInfo["state"], updated: string, title = ""): ThreadInfo => ({
  id, scope, links: [], repo: "", state, started_by: "", created: updated, updated, title,
});

const open = thread(1, "c1", "asleep", "2026-11-23T01:00:00Z", "open in the chat");
const archived = thread(2, "c1", "asleep", "2026-11-23T02:00:00Z", "archived");
const active = thread(3, "c1", "running", "2026-11-23T03:00:00Z", "archived but running");
const elsewhere = thread(4, "c9", "idle", "2026-11-23T03:00:00Z", "a scope with no layout");
const all = [open, archived, active, elsewhere];
// As shows (show.tsx) gives them with the layout's openInChat: under
// Active, a thread open in the chat, or one that works or waits; under All,
// every thread.
const activeOnly = (x: ThreadInfo) => x.id === 1 || ["creating", "running", "waiting", "failed"].includes(x.state);
const everything = () => true;
const title = (q: string) => (x: ThreadInfo) => (x.title ?? "").includes(q);

const layout: ChatLayout[] = [{
  client: "discord",
  groups: [
    { channels: [{ id: "u1", name: "#lobby", url: "https://chat.example/u1", threads: [{ id: "l1", name: "lunch", url: "https://chat.example/l1" }] }] },
    { id: "g1", name: "Work", channels: [{ id: "c1", name: "#builds", scope: "c1", threads: [
      { id: "t1", name: "open in the chat", thread: 1 },
      { id: "p1", name: "a question", url: "https://chat.example/p1" },
    ] }] },
  ],
}];

test("no layout: the scopes as before", () => {
  assert.equal(chatTree(null, all, activeOnly, "", () => true), null);
  assert.equal(chatTree([{ client: "slack", groups: [] }], all, activeOnly, "", () => true), null);
});

test("group, channel, thread in the chat's order; a thread open in the chat whatever its state", () => {
  const tr = chatTree(layout, all, activeOnly, "", () => true)!;
  assert.deepEqual(tr.groups.map((g) => [g.key, g.name, g.channels.map((c) => c.key)]), [
    [undefined, "", ["chan:discord/u1"]],
    ["group:discord/g1", "Work", ["scope:c1"]],
  ]);
  const [lobby] = tr.groups[0].channels;
  assert.deepEqual(lobby.items, [{ kind: "chat", key: "chat:discord/l1", name: "lunch", url: "https://chat.example/l1" }]);
  const [builds] = tr.groups[1].channels;
  // The asleep thread open in the chat, the plain thread, then the scope's
  // threads not open in the chat that the choice shows: not the asleep one.
  assert.deepEqual(builds.items.map((i) => (i.kind === "thread" ? i.x.id : i.key)), [1, "chat:discord/p1", 3]);
  assert.deepEqual(builds.threads.map((x) => x.id), [1, 3]);
  assert.deepEqual([...tr.scopes], ["c1"]);
});

test("what is open in the chat, for the choice of threads", () => {
  assert.equal(openInChat(null), null);
  const open = openInChat(layout)!;
  assert.deepEqual([...open.scopes], ["c1"]);
  assert.deepEqual([...open.threads], [1]);
});

test("All shows the archived thread under its channel, the newest first", () => {
  const tr = chatTree(layout, all, everything, "", () => true)!;
  const [builds] = tr.groups[1].channels;
  assert.deepEqual(builds.items.map((i) => (i.kind === "thread" ? i.x.id : i.key)), [1, "chat:discord/p1", 3, 2]);
});

test("the filter keeps what it finds, and the channels and groups that hold it", () => {
  let tr = chatTree(layout, all, activeOnly, "lunch", title("lunch"))!;
  assert.deepEqual(tr.groups.map((g) => g.channels.map((c) => [c.key, c.items.length])), [[["chan:discord/u1", 1]]]);
  tr = chatTree(layout, all, activeOnly, "lobby", title("lobby"))!;
  assert.deepEqual(tr.groups.map((g) => g.channels.map((c) => [c.key, c.items.length])), [[["chan:discord/u1", 0]]]);
  tr = chatTree(layout, all, activeOnly, "open", title("open"))!;
  assert.deepEqual(tr.groups.map((g) => g.channels.map((c) => c.items.map((i) => (i.kind === "thread" ? i.x.id : i.key)))), [[[1]]]);
});
