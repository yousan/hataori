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
      const session = r.ok ? ((await r.json()) as { session?: unknown }).session : undefined;
      if (typeof session === "string" && session) {
        current = session;
        try {
          localStorage.setItem(storageKey, session);
        } catch {
          // Kept for this tab only.
        }
        return;
      }
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

// authorized returns init with the session's header added.
export function authorized(init: RequestInit = {}): RequestInit {
  const headers = new Headers(init.headers);
  if (current) headers.set("Authorization", `Bearer ${current}`);
  return { ...init, headers, credentials: "omit", cache: "no-store" };
}
