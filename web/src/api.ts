// The routes of the dashboard's server (internal/dashboard/api.go). Their
// JSON is the control protocol's (docs/protocol.md). Every request carries
// the page's session (session.ts).

import { authorized } from "./session";

export type State = "creating" | "idle" | "running" | "waiting" | "asleep" | "closed" | "removed" | "failed";

export const states: readonly State[] = ["creating", "idle", "running", "waiting", "asleep", "closed", "removed", "failed"];

// ThreadID is a thread's number (protocol 3.0).
export type ThreadID = number;

// LinkRef is a conversation a thread is shown in.
export interface LinkRef {
  client: string;
  channel: string;
  thread: string;
}

export interface ThreadInfo {
  id: ThreadID;
  scope: string;
  links: LinkRef[];
  title?: string; // left out when empty, and for a caller that may not read text (protocol 2.0)
  repo: string;
  state: State;
  started_by: string;
  created: string;
  updated: string;
  agent?: string;
  model?: string;
  effort?: string;
  turns?: number;
  last?: string;
  background?: number;
  context?: { used: number; size: number };
  tokens?: { turn: number; today: number };
  terminal?: string;
}

export interface Request {
  id: string;
  kind: number; // 0 a question, 1 an approval
  text?: string;
  choices?: { label: string; token: string }[] | null;
}

export interface Event {
  pos: number;
  thread?: ThreadID; // absent for an event of no thread
  scope?: string;
  time: string;
  kind: string;
  data?: { [field: string]: unknown };
}

export interface ThreadDetail extends ThreadInfo {
  requests: Request[] | null;
  events?: Event[];
}

// AgentLimits is an agent's remaining usage as its sessions last reported
// it; a report older than every seconds is not current.
export interface AgentLimits {
  agent: string;
  at: string;
  every: number;
  windows: LimitWindow[];
}

export interface LimitWindow {
  name: string;
  minutes?: number;
  used: number; // percent
  resets?: string;
}

// SignedOut is thrown when the server answers 401: the session has ended.
export class SignedOut extends Error {}

async function get<T>(path: string): Promise<T> {
  const r = await fetch(path, authorized());
  if (r.status === 401) throw new SignedOut();
  if (!r.ok) throw new Error(`${path}: ${r.status}`);
  return (await r.json()) as T;
}

export function threads(): Promise<ThreadInfo[]> {
  return get<ThreadInfo[]>("/api/threads");
}

export function thread(id: ThreadID, events: number): Promise<ThreadDetail> {
  const q = new URLSearchParams({ thread: String(id), recent: "0", events: String(events) });
  return get<ThreadDetail>(`/api/thread?${q}`);
}

// threadEvents reads a thread's events before position before, or its last
// ones when before is 0: at most limit, newest last (protocol 3.1). An
// empty answer is the thread's start.
export function threadEvents(id: ThreadID, before: number, limit: number): Promise<Event[]> {
  const q = new URLSearchParams({ thread: String(id), limit: String(limit) });
  if (before > 0) q.set("before", String(before));
  return get<Event[]>(`/api/threadevents?${q}`);
}

export function limits(): Promise<AgentLimits[]> {
  return get<AgentLimits[]>("/api/limits");
}

export const eventsPath = "/api/events";

// HostTerminal is a terminal on the host that hataori does not own
// (docs/protocol.md). hataori judges no state of it and sends it nothing.
export interface HostTerminal {
  device: string;
  process?: string;
  activity?: string;
  tmux?: {
    pane: string;
    session: string;
    window: string;
    window_name?: string;
    index: string;
    dir?: string;
    attached: boolean;
  };
}

// HostTerminalsOff is thrown when the server does not list the host's
// terminals: the feature is off, or the caller is not the owner.
export class HostTerminalsOff extends Error {}

export async function hostTerminals(): Promise<HostTerminal[]> {
  const r = await fetch("/api/hostterminals", authorized());
  if (r.status === 401) throw new SignedOut();
  if (r.status === 403) throw new HostTerminalsOff();
  if (!r.ok) throw new Error(`hostterminals: ${r.status}`);
  return (await r.json()) as HostTerminal[];
}

export function hostScreenPath(pane: string): string {
  return `/api/hostscreen?${new URLSearchParams({ pane })}`;
}

// WorkspaceState is what a thread's workspace holds (docs/protocol.md).
export interface WorkspaceState {
  branch: string; // empty when no branch is checked out
  base: string;
  added: number;
  removed: number;
  uncommitted: number;
  unpushed: number;
  fetched?: string;
}

// workspace reads a thread's workspace. A thread that is gone, or a
// workspace that cannot be read, is an error: the page shows no numbers
// for it rather than zeros.
export async function workspace(id: ThreadID): Promise<WorkspaceState> {
  return get<WorkspaceState>(`/api/workspace?${new URLSearchParams({ thread: String(id) })}`);
}

// TerminalChunk is one message of a terminal stream (docs/protocol.md).
export interface TerminalChunk {
  run: string; // empty when no host runs
  pos: number;
  reset: boolean;
  cols: number;
  rows: number;
  data?: string; // base64
}

export function terminalPath(id: ThreadID): string {
  return `/api/terminal?${new URLSearchParams({ thread: String(id) })}`;
}

// hasTerminal reports whether a thread's session has a terminal stream:
// a session on pipes answers 404. It asks with HEAD, which answers with
// the stream's status and sends no chunk. Any other answer, or none, is an
// error: the caller asks again.
export async function hasTerminal(id: ThreadID): Promise<boolean> {
  const r = await fetch(terminalPath(id), authorized({ method: "HEAD" }));
  if (r.status === 401) throw new SignedOut();
  if (r.status === 404) return false;
  if (!r.ok) throw new Error(`terminal: ${r.status}`);
  return true;
}

export function sameThread(a: ThreadID | undefined, b: ThreadID): boolean {
  return a === b;
}
