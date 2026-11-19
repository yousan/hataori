import { authorized } from "./session";

// Stream reads a Server-Sent Events stream with fetch, which can send the
// session's header; EventSource cannot. It behaves as EventSource does
// where the page relies on it: messages with their data, type and last
// event id; onopen when a response starts; onerror and a reconnect with
// Last-Event-ID after the stream ends or a connection fails; and no
// reconnect after an answer that is not a stream (such as 401), where
// readyState becomes CLOSED.
//
// The request carries no body and sends nothing back: a stream only reads.

export interface StreamMessage {
  type: string; // "message" unless the stream named an event
  data: string;
  lastEventId: string;
}

export class Stream {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;

  readyState: number = Stream.CONNECTING;
  onopen: (() => void) | null = null;
  onmessage: ((m: StreamMessage) => void) | null = null;
  onerror: (() => void) | null = null;

  private listeners = new Map<string, ((m: StreamMessage) => void)[]>();
  private abort: AbortController | null = null;
  private timer: ReturnType<typeof setTimeout> | undefined;
  private lastEventId = "";
  private retry = 1000;

  constructor(private readonly url: string) {
    this.connect();
  }

  addEventListener(type: string, f: (m: StreamMessage) => void): void {
    const fs = this.listeners.get(type) ?? [];
    fs.push(f);
    this.listeners.set(type, fs);
  }

  close(): void {
    this.readyState = Stream.CLOSED;
    clearTimeout(this.timer);
    this.abort?.abort();
  }

  private async connect(): Promise<void> {
    if (this.readyState === Stream.CLOSED) return;
    this.readyState = Stream.CONNECTING;
    const abort = new AbortController();
    this.abort = abort;
    const headers: Record<string, string> = { Accept: "text/event-stream" };
    if (this.lastEventId) headers["Last-Event-ID"] = this.lastEventId;
    let r: Response;
    try {
      r = await fetch(this.url, authorized({ headers, signal: abort.signal }));
    } catch {
      this.lost();
      return;
    }
    if (this.readyState === Stream.CLOSED) return;
    const type = r.headers.get("Content-Type") ?? "";
    if (!r.ok || !type.startsWith("text/event-stream") || !r.body) {
      // As EventSource: an answer that is not a stream does not come back.
      this.readyState = Stream.CLOSED;
      r.body?.cancel().catch(() => {});
      call(this.onerror);
      return;
    }
    this.readyState = Stream.OPEN;
    call(this.onopen);
    try {
      await this.read(r.body);
    } catch {
      // A dropped connection: reconnect below.
    }
    abort.abort(); // let go of the connection, whatever ended the read
    this.lost();
  }

  // lost reports the end of a stream and reconnects after the retry delay.
  private lost(): void {
    if (this.readyState === Stream.CLOSED) return;
    this.readyState = Stream.CONNECTING;
    call(this.onerror);
    if (this.readyState === Stream.CLOSED) return; // closed by onerror
    this.timer = setTimeout(() => this.connect(), this.retry);
  }

  private async read(body: ReadableStream<Uint8Array>): Promise<void> {
    const reader = body.getReader();
    const decoder = new TextDecoder();
    let buf = "";
    let data: string[] = [];
    let event = "";
    let id: string | null = null;
    const dispatch = () => {
      if (id !== null) this.lastEventId = id;
      id = null;
      if (data.length === 0) {
        event = "";
        return;
      }
      const m: StreamMessage = { type: event || "message", data: data.join("\n"), lastEventId: this.lastEventId };
      data = [];
      event = "";
      if (this.readyState === Stream.CLOSED) return;
      if (m.type === "message") call(this.onmessage, m);
      for (const f of this.listeners.get(m.type) ?? []) call(f, m);
    };
    for (;;) {
      const { value, done } = await reader.read();
      if (done) return;
      buf += decoder.decode(value, { stream: true });
      for (;;) {
        const nl = buf.search(/\r\n|\r|\n/);
        if (nl < 0) break;
        // A lone \r at the end may be the first half of \r\n: wait for more.
        if (buf[nl] === "\r" && nl === buf.length - 1) break;
        const line = buf.slice(0, nl);
        buf = buf.slice(buf.startsWith("\r\n", nl) ? nl + 2 : nl + 1);
        if (line === "") {
          dispatch();
          continue;
        }
        if (line.startsWith(":")) continue; // a comment, such as a keep-alive
        const colon = line.indexOf(":");
        const field = colon < 0 ? line : line.slice(0, colon);
        let value = colon < 0 ? "" : line.slice(colon + 1);
        if (value.startsWith(" ")) value = value.slice(1);
        switch (field) {
          case "data":
            data.push(value);
            break;
          case "event":
            event = value;
            break;
          case "id":
            if (!value.includes("\0")) id = value;
            break;
          case "retry":
            if (/^\d+$/.test(value)) this.retry = Number(value);
            break;
        }
      }
    }
  }
}

// call runs a handler of the page's. As with EventSource, what a handler
// throws does not touch the stream: it is reported, and reading goes on.
function call<A extends unknown[]>(f: ((...a: A) => void) | null | undefined, ...a: A): void {
  try {
    f?.(...a);
  } catch (err) {
    queueMicrotask(() => {
      throw err;
    });
  }
}
