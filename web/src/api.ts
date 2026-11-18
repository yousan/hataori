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

export function limits(): Promise<AgentLimits[]> {
  return get<AgentLimits[]>("/api/limits");
}

export const eventsPath = "/api/events";

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
  return get<WorkspaceState>(`/api/workspace?${new URLSearchParams({ scope: id.scope, key: id.key })}`);
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
  return `/api/terminal?${new URLSearchParams({ scope: id.scope, key: id.key })}`;
}

// hasTerminal reports whether a thread's session has a terminal stream:
// a session on pipes answers 404. It asks with HEAD, which answers with
// the stream's status and sends no chunk. Any other answer, or none, is an
// error: the caller asks again.
export async function hasTerminal(id: ThreadID): Promise<boolean> {
  const r = await fetch(terminalPath(id), { method: "HEAD", credentials: "same-origin", cache: "no-store" });
  if (r.status === 401) throw new SignedOut();
  if (r.status === 404) return false;
  if (!r.ok) throw new Error(`terminal: ${r.status}`);
  return true;
}

export function sameThread(a: ThreadID, b: ThreadID): boolean {
  return a.scope === b.scope && a.key === b.key;
}
