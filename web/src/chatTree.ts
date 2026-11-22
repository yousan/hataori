// The tree as the chat shows it (design, section 7.2; #599): group, then
// channel, then thread, in the chat's order, from the layout the chat
// front end records (protocol 3.21). A channel hataori serves holds its
// hataori threads that are open in the chat, as rows, and the chat's other
// open threads, as links into the chat; a channel hataori does not serve
// is a link with its open threads. A hataori thread whose chat thread is
// not open (archived) is shown under its channel only when the choice of
// threads shows it. With no layout, the tree is the scopes, as before.
//
// This module only builds the model; Tree.tsx draws it. It imports no
// module that reads the browser, so that node's tests can load it.

import type { ChatLayout, ThreadInfo } from "./api";
import type { OpenInChat } from "./show";

// ChatItem is one thing under a channel: a hataori thread, drawn as its
// row, or another open thread of the chat, drawn as a dimmed link.
export type ChatItem =
  | { kind: "thread"; x: ThreadInfo }
  | { kind: "chat"; key: string; name: string; url?: string };

// ChatChannel is a channel. A served one (scope set) is the scope's node,
// with today's count and dots for its hataori threads.
export interface ChatChannel {
  key: string; // its tree item: "scope:<key>" for a served channel, "chan:<client>/<id>" else
  scope?: string;
  name: string;
  url?: string;
  items: ChatItem[];
  threads: ThreadInfo[]; // the hataori threads among items, in their order
}

// ChatGroup is a group of channels; the channels in no group have no key
// and are drawn without a node of their own.
export interface ChatGroup {
  key?: string; // "group:<client>/<id>"
  name: string;
  channels: ChatChannel[];
}

export interface ChatTree {
  groups: ChatGroup[];
  scopes: ReadonlySet<string>; // the scopes whose channel is in the layout; the others are drawn as before
}

// chatTree is the tree's model from layouts, or null when no client
// reported one (a surface without a layout): the page then draws the
// scopes as before. all is every thread the caller may see; shown says
// whether the choice of threads shows a thread (shows, with openInChat);
// matches whether the filter q finds a thread.
export function chatTree(
  layouts: readonly ChatLayout[] | null,
  all: readonly ThreadInfo[],
  shown: (x: ThreadInfo) => boolean,
  q: string,
  matches: (x: ThreadInfo) => boolean,
): ChatTree | null {
  const ls = (layouts ?? []).filter((l) => l.groups.length > 0);
  if (ls.length === 0) return null;
  const filtering = q.trim() !== "";
  const f = q.trim().toLowerCase();
  const named = (name: string | undefined) => !filtering || (name ?? "").toLowerCase().includes(f);
  const byID = new Map(all.map((x) => [x.id, x]));
  const inChat = new Set<number>();
  const scopes = new Set<string>();
  for (const l of ls)
    for (const g of l.groups)
      for (const c of g.channels) {
        if (c.scope) scopes.add(c.scope);
        for (const th of c.threads) if (ownThread(th.thread, c.scope, byID)) inChat.add(th.thread!);
      }
  const groups: ChatGroup[] = [];
  for (const l of ls) {
    for (const g of l.groups) {
      const channels: ChatChannel[] = [];
      for (const c of g.channels) {
        const items: ChatItem[] = [];
        for (const th of c.threads) {
          // A hataori thread shows as its row only in its own scope's
          // channel; linked into another (show_in), it is that channel's
          // other open thread there.
          const x = ownThread(th.thread, c.scope, byID) ? byID.get(th.thread!) : undefined;
          if (x) {
            if (shown(x) && matches(x)) items.push({ kind: "thread", x });
          } else if (named(th.name)) {
            items.push({ kind: "chat", key: `chat:${l.client}/${th.id}`, name: th.name ?? "", url: th.url });
          }
        }
        if (c.scope) {
          // The scope's threads that are not open in the chat, as the
          // choice of threads shows them, the newest first.
          const rest = all
            .filter((x) => x.scope === c.scope && !inChat.has(x.id) && shown(x) && matches(x))
            .sort((a, b) => Date.parse(b.updated) - Date.parse(a.updated) || b.id - a.id);
          for (const x of rest) items.push({ kind: "thread", x });
        }
        if (filtering && items.length === 0 && (c.scope || !named(c.name))) continue;
        channels.push({
          key: c.scope ? `scope:${c.scope}` : `chan:${l.client}/${c.id}`,
          scope: c.scope,
          name: c.name ?? "",
          url: c.url,
          items,
          threads: items.flatMap((i) => (i.kind === "thread" ? [i.x] : [])),
        });
      }
      if (channels.length === 0) continue;
      groups.push({ key: g.id ? `group:${l.client}/${g.id}` : undefined, name: g.name ?? "", channels });
    }
  }
  return { groups, scopes };
}

// ownThread reports whether thread n is one of all, of scope.
function ownThread(n: number | undefined, scope: string | undefined, byID: ReadonlyMap<number, ThreadInfo>): boolean {
  return n !== undefined && scope !== undefined && byID.get(n)?.scope === scope;
}

// openInChat is what layouts say is open in the chat, for the choice of
// threads (show.tsx): the scopes whose channel they lay out and the
// threads of all open in a channel of their own scope; null without a
// layout.
export function openInChat(layouts: readonly ChatLayout[] | null, all: readonly ThreadInfo[]): OpenInChat | null {
  const ls = (layouts ?? []).filter((l) => l.groups.length > 0);
  if (ls.length === 0) return null;
  const byID = new Map(all.map((x) => [x.id, x]));
  const scopes = new Set<string>();
  const threads = new Set<number>();
  for (const l of ls)
    for (const g of l.groups)
      for (const c of g.channels) {
        if (c.scope) scopes.add(c.scope);
        for (const th of c.threads) if (ownThread(th.thread, c.scope, byID)) threads.add(th.thread!);
      }
  return { scopes, threads };
}
