// A thread's conversation, from its event log: what each event of the
// kinds a person reads becomes, in order. The page infers nothing else from
// the events.

import type { Event } from "./api";

// conversationKinds are the events a conversation is made of.
export const conversationKinds: ReadonlySet<string> = new Set([
  "turn_started",
  "input",
  "request",
  "answer",
  "text",
  "tool_started",
  "tool_ended",
  "turn_ended",
  "start_chosen",
]);

// Entry is one line of a conversation. A text that is absent was withheld
// from the caller (protocol 2.0): the page says so instead.
export type Entry =
  | { kind: "turn_started"; pos: number; time: string; by: string; who?: string }
  | { kind: "input"; pos: number; time: string; who?: string; text?: string }
  | { kind: "request"; pos: number; time: string; approval: boolean; text?: string; choices?: string[] }
  | { kind: "answer"; pos: number; time: string; who?: string; text?: string }
  | { kind: "text"; pos: number; time: string; final: boolean; text?: string }
  | { kind: "tool"; pos: number; time: string; tool: string; parent?: string; ended?: string }
  | { kind: "turn_ended"; pos: number; time: string; outcome: string; reason?: string }
  | { kind: "start_chosen"; pos: number; time: string; choice: string; who?: string };

const str = (v: unknown): string | undefined => (typeof v === "string" ? v : undefined);
const num = (v: unknown): number | undefined => (typeof v === "number" ? v : undefined);

// who is the person an input, answer, turn or choice came from: their name, else the
// chat id it came through.
export function who(d: { [field: string]: unknown }): string | undefined {
  return str(d.principal) || str(d.from) || undefined;
}

// entries makes the conversation of events, which are in order and each
// once. A tool call is one entry at its start; its end, when read, closes
// it. An end whose start is not read yet (older than what is loaded) shows
// nothing until the start is read.
export function entries(events: readonly Event[]): Entry[] {
  const out: Entry[] = [];
  const open = new Map<string, Extract<Entry, { kind: "tool" }>>();
  for (const e of events) {
    const d = e.data ?? {};
    const base = { pos: e.pos, time: e.time };
    switch (e.kind) {
      case "turn_started":
        out.push({ kind: "turn_started", ...base, by: str(d.by) ?? "", who: who(d) });
        break;
      case "input":
        out.push({ kind: "input", ...base, who: who(d), text: str(d.text) });
        break;
      case "request":
        out.push({
          kind: "request",
          ...base,
          approval: d.approval === true,
          text: str(d.text),
          choices: Array.isArray(d.choices) ? d.choices.filter((c): c is string => typeof c === "string") : undefined,
        });
        break;
      case "answer":
        out.push({ kind: "answer", ...base, who: who(d), text: str(d.text) });
        break;
      case "text":
        out.push({ kind: "text", ...base, final: d.final === true, text: str(d.text) });
        break;
      case "tool_started": {
        const tool: Extract<Entry, { kind: "tool" }> = { kind: "tool", ...base, tool: str(d.tool) ?? "", parent: str(d.parent) };
        open.set(`${num(d.session)}:${str(d.id)}`, tool);
        out.push(tool);
        break;
      }
      case "tool_ended": {
        const key = `${num(d.session)}:${str(d.id)}`;
        const tool = open.get(key);
        if (tool) {
          tool.ended = e.time;
          open.delete(key);
        }
        break;
      }
      case "turn_ended":
        out.push({ kind: "turn_ended", ...base, outcome: str(d.outcome) ?? "", reason: str(d.reason) || undefined });
        break;
      case "start_chosen":
        out.push({ kind: "start_chosen", ...base, choice: str(d.choice) ?? "", who: who(d) });
        break;
    }
  }
  return out;
}

// merge is have and more together: each event of the conversation's kinds
// once, by position, in order.
export function merge(have: Event[], more: readonly Event[]): Event[] {
  const byPos = new Map(have.map((e) => [e.pos, e]));
  let added = false;
  for (const e of more) {
    if (byPos.has(e.pos) || !conversationKinds.has(e.kind)) continue;
    byPos.set(e.pos, e);
    added = true;
  }
  return added ? [...byPos.values()].sort((a, b) => a.pos - b.pos) : have;
}
