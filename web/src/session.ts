// The page's session: a token the server signed, kept in this origin's own
// storage and sent in the Authorization header of every request.
//
// It is not a cookie. A browser sends a cookie to every port of the host
// that set it, so a service someone else runs on another port of the same
// machine would get it. Storage is kept apart by scheme, host and port.
// The token never goes into an address: a reverse proxy's access log keeps
// addresses.

const storageKey = "hataori-session";
// signInPath is the page a sign-in link opens; the link's token is in the
// address's fragment, which no server and no preview crawler sees
// (internal/dashboard, SignInPath and SessionPath).
const signInPath = "/signin";
const sessionPath = "/api/session";

let current: string | null = null;
// refused is true when this page was opened with a sign-in link that did
// not sign in: used already, by the person or by someone else, or expired.
let refused = false;

// SignedIn is what a sign-in answers: the session and, on the web
// listener, the proof that registers one passkey within its seconds.
export interface SignedIn {
  session?: unknown;
  register?: { token?: unknown; expires_in?: unknown };
}

// proof is the registration proof of the last sign-in, kept in memory
// only: it is good for one passkey, for a few minutes, with this session.
// from says which sign-in brought it.
let proof: { token: string; until: number; from: "link" | "passkey" } | null = null;
const proofWatchers = new Set<() => void>();

// registerProof returns the proof's token while it is good, else null.
export function registerProof(): string | null {
  return proof && Date.now() < proof.until ? proof.token : null;
}

// linkProofUntil is when the proof a sign-in link brought stops being
// good, in milliseconds since the epoch; 0 when there is none.
export function linkProofUntil(): number {
  return proof?.from === "link" ? proof.until : 0;
}

// dropProof forgets the proof: it was used, or the person declined.
export function dropProof(): void {
  proof = null;
  for (const f of proofWatchers) f();
}

// watchProof calls f whenever the proof changes, until the returned
// function is called.
export function watchProof(f: () => void): () => void {
  proofWatchers.add(f);
  return () => proofWatchers.delete(f);
}

// adopt keeps what a sign-in answered: its session, in this origin's
// storage, and its registration proof, in memory. It reports whether there
// was a session.
export function adopt(a: SignedIn, from: "link" | "passkey"): boolean {
  if (typeof a.session !== "string" || !a.session) return false;
  current = a.session;
  try {
    localStorage.setItem(storageKey, a.session);
  } catch {
    // Kept for this tab only.
  }
  const r = a.register;
  proof =
    r && typeof r.token === "string" && typeof r.expires_in === "number"
      ? { token: r.token, until: Date.now() + r.expires_in * 1000, from }
      : null;
  for (const f of proofWatchers) f();
  return true;
}

// linkRefused reports whether the page was opened with a sign-in link that
// did not sign in, so that it can say so instead of only "signed out".
export function linkRefused(): boolean {
  return refused;
}

// adoptSession finds the session once, before the page asks for anything.
// On a sign-in link it takes the link's token out of the address at once,
// and trades it for a session, which it keeps; a link that was used or has
// expired leaves the session kept earlier, if any. Otherwise it is the one
// kept earlier in this origin's storage. Storage may be unavailable (a
// private window may refuse it); the session then lasts as long as the tab.
export async function adoptSession(): Promise<void> {
  let link: string | null = null;
  if (location.pathname === signInPath) {
    link = new URLSearchParams(location.hash.slice(1)).get("t");
    history.replaceState(null, "", "/");
  }
  if (link) {
    try {
      const r = await fetch(sessionPath, {
        headers: { Authorization: `Link ${link}` },
        credentials: "omit",
        cache: "no-store",
      });
      if (r.status === 401) refused = true; // used, by anyone, or expired; not a network failure
      if (r.ok && adopt((await r.json()) as SignedIn, "link")) return;
    } catch {
      // No session from this link: the one kept earlier, below.
    }
  }
  try {
    current = localStorage.getItem(storageKey);
  } catch {
    current = null;
  }
}

// sessionToken is the page's session, for the one connection that cannot
// carry it in a header: the operate WebSocket sends it as its first
// message (operate.ts).
export function sessionToken(): string | null {
  return current;
}

// authorized returns init with the session's header added.
export function authorized(init: RequestInit = {}): RequestInit {
  const headers = new Headers(init.headers);
  if (current) headers.set("Authorization", `Bearer ${current}`);
  return { ...init, headers, credentials: "omit", cache: "no-store" };
}
