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

// ThreadLink is a link of a thread, and whether its client opened the conversation (protocol 3.40).
export interface ThreadLink extends LinkRef {
  opened?: boolean;
}

export interface ThreadInfo {
  id: ThreadID;
  scope: string;
  links: ThreadLink[];
  title?: string; // left out when empty, and for a caller that may not read text (protocol 2.0)
  name?: string; // the chat thread's own name, following a rename; text, so left out the same way (protocol 3.7)
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
  confined?: boolean; // a supervisor's thread: its directory holds no repository (protocol 3.36)
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
  failed_start?: FailedStart;
}

// FailedStart is a session that failed before its first turn, or when it
// was woken (protocol 3.33). Its texts are left out for a caller that may
// not read text.
export interface FailedStart {
  session: number;
  event: number; // the position of the state event of the failure
  reason?: string;
  detail?: string;
  trust?: string[]; // the files a start-up dialog named
  text?: string; // what the thread was asked, while it had no turn
  supervisor?: ThreadID; // the supervisor thread that started it
}

// AgentLimits is an agent's remaining usage as its sessions last reported
// it; a report older than every seconds is not current.
export interface AgentLimits {
  agent: string;
  at: string;
  every: number;
  windows: LimitWindow[];
}

// ClientState is whether a chat client of the caller's scopes is connected
// to its chat service, and since when it is or is not (protocol 3.21).
export interface ClientState {
  client: string;
  connected: boolean;
  since: string;
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

// ScopeInfo is a scope the caller may see, with the name a person knows it
// by: the config's, else its channel's as the chat service shows it; absent
// when none is known (protocol 3.5).
export interface ScopeInfo {
  key: string;
  name?: string;
}

export function scopes(): Promise<ScopeInfo[]> {
  return get<ScopeInfo[]>("/api/scopes");
}

// ChatLayout is how one chat client's service lays out the conversations
// it can see, as a person sees them in its own client: groups (Discord's
// categories), the channels in order, and each channel's open threads in
// order (protocol 3.21). A client that reports none has no entry.
export interface ChatLayout {
  client: string;
  groups: LayoutGroup[];
}

export interface LayoutGroup {
  id?: string; // absent for the channels in no group
  name?: string;
  channels: LayoutChannel[];
}

export interface LayoutChannel {
  id: string;
  name?: string; // text: absent for a caller that may not read text
  url?: string; // opens the channel in the chat; text, as name
  scope?: string; // the scope whose own channel this is; absent for a channel hataori does not serve
  threads: LayoutThread[];
}

export interface LayoutThread {
  id: string;
  name?: string;
  url?: string;
  thread?: ThreadID; // the thread hataori holds in this conversation; absent for one it does not
}

export function layout(): Promise<ChatLayout[]> {
  return get<ChatLayout[]>("/api/layout");
}

export function threads(): Promise<ThreadInfo[]> {
  return get<ThreadInfo[]>("/api/threads");
}

export function thread(id: ThreadID, events: number): Promise<ThreadDetail> {
  const q = new URLSearchParams({ thread: String(id), recent: "0", events: String(events) });
  return get<ThreadDetail>(`/api/thread?${q}`);
}

// threadEvents reads a thread's events before position before, or its last
// ones when before is 0: at most limit, newest last (protocol 3.3). An
// empty answer is the thread's start.
export function threadEvents(id: ThreadID, before: number, limit: number): Promise<Event[]> {
  const q = new URLSearchParams({ thread: String(id), limit: String(limit) });
  if (before > 0) q.set("before", String(before));
  return get<Event[]>(`/api/threadevents?${q}`);
}

export function limits(): Promise<AgentLimits[]> {
  return get<AgentLimits[]>("/api/limits");
}

export function clients(): Promise<ClientState[]> {
  return get<ClientState[]>("/api/clients");
}

// Me is the person of the page's session and how they signed in: on which
// listener ("local", this machine, or "web"), and whether with a passkey
// rather than a sign-in link.
export interface Me {
  person: string;
  way: string;
  passkey: boolean;
}

export function me(): Promise<Me> {
  return get<Me>("/api/me");
}

// TrustedFile is a file the owner trusted for a scope's threads on one
// repository, from a blocked start (docs/protocol.md). Since 3.28.
export interface TrustedFile {
  id: number;
  scope: string;
  repo: string;
  file: string;
  by: string;
  at: string;
}

export function trusts(): Promise<TrustedFile[]> {
  return get<TrustedFile[]>("/api/trusts");
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
  // reported is what the hooks of the session in this tmux pane reported,
  // through hataori's Claude Code plugin: no text of the session, and no
  // state hataori judged. Since protocol 3.15.
  reported?: ReportedSession;
}

// ReportedSession is what a session a person started last reported
// (docs/protocol.md).
export interface ReportedSession {
  window: string;
  agent: string;
  session: string;
  dir: string;
  event: string;
  at: string;
  thread?: number;
}

// HostTerminalsOff is thrown when the server does not list the host's
// terminals: the feature is off, or the caller is not the owner.
export class HostTerminalsOff extends Error {}

// hostTerminals lists the host's terminals, with skew: how far the host's
// clock, as its answer's Date header gives it, is ahead of this browser's,
// in milliseconds; 0 when the answer does not say. Their times are the
// host's.
export async function hostTerminals(): Promise<{ list: HostTerminal[]; skew: number }> {
  const r = await fetch("/api/hostterminals", authorized());
  if (r.status === 401) throw new SignedOut();
  if (r.status === 403) throw new HostTerminalsOff();
  if (!r.ok) throw new Error(`hostterminals: ${r.status}`);
  const date = Date.parse(r.headers.get("Date") ?? "");
  const skew = Number.isFinite(date) ? date - Date.now() : 0;
  return { list: (await r.json()) as HostTerminal[], skew };
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
