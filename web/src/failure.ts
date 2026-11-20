// failureReason is what the page shows for a session that failed: the
// state event's reason, and its detail when that is one line of text for a
// person, as the chat front end posts it (internal/chat, ReadableDetail).
// A detail with control characters or line breaks is terminal output, such
// as the screen of a start that timed out; the events list shows it.

// maxDetail bounds the detail shown with the reason.
const maxDetail = 1000;

export function readableDetail(detail: unknown): string | undefined {
  if (typeof detail !== "string") return undefined;
  const d = detail.trim();
  // The same bound as the server's, which counts bytes.
  if (d === "" || new TextEncoder().encode(d).length > maxDetail) return undefined;
  for (const ch of d) {
    const c = ch.codePointAt(0) ?? 0;
    if (c < 0x20 || c === 0x7f || (c >= 0x80 && c < 0xa0)) return undefined;
  }
  return d;
}

export function failureReason(data: Record<string, unknown> | undefined): string | undefined {
  const r = data?.reason;
  if (typeof r !== "string" || r === "") return undefined;
  const d = readableDetail(data?.detail);
  return d ? `${r}: ${d}` : r;
}
