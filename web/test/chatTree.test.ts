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
  // The plain thread, then the scope's threads not open in the chat that
  // the choice shows; the asleep ones, open in the chat or not, apart.
  assert.deepEqual(builds.items.map((i) => (i.kind === "thread" ? i.x.id : i.key)), ["chat:discord/p1", 3]);
  assert.deepEqual(builds.threads.map((x) => x.id), [3]);
  assert.deepEqual(builds.archived.map((x) => x.id), [2, 1]);
  assert.deepEqual(lobby.archived, []);
  assert.deepEqual([...tr.scopes], ["c1"]);
});

test("what is open in the chat, for the choice of threads", () => {
  assert.equal(openInChat(null, all), null);
  const open = openInChat(layout, all)!;
  assert.deepEqual([...open.scopes], ["c1"]);
  assert.deepEqual([...open.threads], [1]);
});

test("All shows a thread not open in the chat under its channel; asleep ones stay apart", () => {
  const closed = thread(6, "c1", "closed", "2026-11-23T05:00:00Z", "closed");
  const tr = chatTree(layout, [...all, closed], everything, "", () => true)!;
  const [builds] = tr.groups[1].channels;
  assert.deepEqual(builds.items.map((i) => (i.kind === "thread" ? i.x.id : i.key)), ["chat:discord/p1", 6, 3]);
  assert.deepEqual(builds.archived.map((x) => x.id), [2, 1]);
});

test("the filter keeps what it finds, and the channels and groups that hold it", () => {
  let tr = chatTree(layout, all, activeOnly, "lunch", title("lunch"))!;
  assert.deepEqual(tr.groups.map((g) => g.channels.map((c) => [c.key, c.items.length])), [[["chan:discord/u1", 1]]]);
  tr = chatTree(layout, all, activeOnly, "lobby", title("lobby"))!;
  assert.deepEqual(tr.groups.map((g) => g.channels.map((c) => [c.key, c.items.length])), [[["chan:discord/u1", 0]]]);
  tr = chatTree(layout, all, activeOnly, "open", title("open"))!;
  assert.deepEqual(tr.groups.map((g) => g.channels.map((c) => [c.items.length, c.archived.map((x) => x.id)])), [[[0, [1]]]]);
});

test("a thread linked into another channel is a row only in its own scope", () => {
  const linked = thread(5, "c1", "running", "2026-11-23T04:00:00Z", "linked elsewhere");
  const ts = [...all, linked];
  const l: ChatLayout[] = [{
    client: "discord",
    groups: [{ channels: [
      { id: "c1", name: "#builds", scope: "c1", threads: [{ id: "t1", name: "open in the chat", thread: 1 }] },
      { id: "c2", name: "#deploys", scope: "c2", threads: [{ id: "t5", name: "linked elsewhere", thread: 5 }] },
      { id: "u1", name: "#lobby", threads: [{ id: "t6", name: "linked elsewhere", thread: 5 }] },
    ] }],
  }];
  const open = openInChat(l, ts)!;
  assert.deepEqual([...open.threads], [1]);
  const shown = (x: ThreadInfo) => open.threads.has(x.id) || ["creating", "running", "waiting", "failed"].includes(x.state);
  const tr = chatTree(l, ts, shown, "", () => true)!;
  const items = tr.groups[0].channels.map((c) => [c.key, c.items.map((i) => (i.kind === "thread" ? i.x.id : i.key))]);
  assert.deepEqual(items, [
    ["scope:c1", [5, 3]], // its own scope's row, as a thread not open there that works; 1 is asleep
    ["scope:c2", ["chat:discord/t5"]],
    ["chan:discord/u1", ["chat:discord/t6"]],
  ]);
});
