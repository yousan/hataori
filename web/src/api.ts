// The routes of the dashboard's server (internal/dashboard/api.go). Their
// JSON is the control protocol's (docs/protocol.md).

export type State = "creating" | "idle" | "running" | "waiting" | "asleep" | "closed" | "removed" | "failed";

export const states: readonly State[] = ["creating", "idle", "running", "waiting", "asleep", "closed", "removed", "failed"];

export interface ThreadID {
  scope: string;
  key: string;
}

export interface ThreadInfo {
  id: ThreadID;
  number: number;
  title: string;
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
}

export interface Event {
  pos: number;
  thread: ThreadID;
  time: string;
  kind: string;
  data?: { [field: string]: unknown };
}

export interface ThreadDetail extends ThreadInfo {
  requests: Request[] | null;
  events?: Event[];
}

// SignedOut is thrown when the server answers 401: the session has ended.
export class SignedOut extends Error {}

async function get<T>(path: string): Promise<T> {
  const r = await fetch(path, { credentials: "same-origin", cache: "no-store" });
  if (r.status === 401) throw new SignedOut();
  if (!r.ok) throw new Error(`${path}: ${r.status}`);
  return (await r.json()) as T;
}

export function threads(): Promise<ThreadInfo[]> {
  return get<ThreadInfo[]>("/api/threads");
}

export function thread(id: ThreadID, events: number): Promise<ThreadDetail> {
  const q = new URLSearchParams({ scope: id.scope, key: id.key, recent: "0", events: String(events) });
  return get<ThreadDetail>(`/api/thread?${q}`);
}

export const eventsPath = "/api/events";

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
  return `/api/terminal?${new URLSearchParams({ scope: id.scope, key: id.key })}`;
}

// hasTerminal reports whether a thread's session has a terminal stream:
// a session on pipes answers 404. It asks with HEAD, which answers with
// the stream's status and sends no chunk.
export async function hasTerminal(id: ThreadID): Promise<boolean> {
  const r = await fetch(terminalPath(id), { method: "HEAD", credentials: "same-origin", cache: "no-store" });
  if (r.status === 401) throw new SignedOut();
  return r.ok;
}

export function sameThread(a: ThreadID, b: ThreadID): boolean {
  return a.scope === b.scope && a.key === b.key;
}
