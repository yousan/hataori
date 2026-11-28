// The native bridge exposes only allowlisted duplex dashboard operations.
// Browser pages keep their ordinary WebSocket. Native pages fail closed when
// the isolated preload is absent; they never try an owner TCP endpoint.
interface NativeEvent {
  type: "open" | "message" | "error" | "close";
  data?: string;
  code?: number;
  reason?: string;
}
interface NativeConnection {
  send(text: string): Promise<void>;
  close(): void;
}
interface NativeBridge {
  connect(path: string, receive: (event: NativeEvent) => void): NativeConnection;
}
declare global {
  interface Window { hataoriNative?: NativeBridge }
}

export function dashboardSocket(path: string): WebSocket {
  if (location.origin !== "hataori-desktop://instance") {
    return new WebSocket(`${location.protocol === "https:" ? "wss:" : "ws:"}//${location.host}${path}`);
  }
  if (path !== "/api/attach" && path !== "/api/operate" || !window.hataoriNative) throw new Error("Native transport unavailable");
  return new NativeSocket(path, window.hataoriNative);
}

class NativeSocket extends EventTarget implements WebSocket {
  readonly CONNECTING = 0;
  readonly OPEN = 1;
  readonly CLOSING = 2;
  readonly CLOSED = 3;
  readonly extensions = "";
  readonly protocol = "";
  readonly url: string;
  binaryType: BinaryType = "blob";
  bufferedAmount = 0;
  readyState: 0 | 1 | 2 | 3 = this.CONNECTING;
  onopen: WebSocket["onopen"] = null;
  onmessage: WebSocket["onmessage"] = null;
  onerror: WebSocket["onerror"] = null;
  onclose: WebSocket["onclose"] = null;
  private connection: NativeConnection;
  private writing: Promise<void> = Promise.resolve();

  constructor(path: string, bridge: NativeBridge) {
    super();
    this.url = `hataori-desktop://instance${path}`;
    this.connection = bridge.connect(path, (event) => {
      if (this.readyState === this.CLOSED) return;
      if (event.type === "open") {
        if (this.readyState !== this.CONNECTING) return;
        this.readyState = this.OPEN;
        const e = new Event("open");
        this.onopen?.call(this, e);
        this.dispatchEvent(e);
      } else if (event.type === "message" && this.readyState === this.OPEN && typeof event.data === "string") {
        const e = new MessageEvent("message", { data: event.data });
        this.onmessage?.call(this, e);
        this.dispatchEvent(e);
      } else if (event.type === "error") {
        this.fail();
      } else if (event.type === "close") {
        this.ended(event.code ?? 1006, event.reason ?? "");
      }
    });
  }

  send(data: string | ArrayBufferLike | Blob | ArrayBufferView): void {
    if (this.readyState !== this.OPEN) throw new DOMException("Socket is not open", "InvalidStateError");
    if (typeof data !== "string") throw new TypeError("Native duplex messages must be text");
    const length = new TextEncoder().encode(data).byteLength;
    if (length > 65536 || this.bufferedAmount + length > 262144) {
      this.fail();
      return;
    }
    this.bufferedAmount += length;
    // Ordered writes are acknowledged only after the socket accepts them.
    // Failure discards the queue; another connection never inherits input.
    this.writing = this.writing.then(async () => {
      if (this.readyState !== this.OPEN) return;
      await this.connection.send(data);
      if (this.readyState === this.OPEN) this.bufferedAmount -= length;
    }).catch(() => this.fail());
  }

  close(): void {
    if (this.readyState === this.CLOSED) return;
    this.readyState = this.CLOSING;
    this.connection.close();
    this.ended(1000, "");
  }

  private fail(): void {
    if (this.readyState === this.CLOSED) return;
    const e = new Event("error");
    this.onerror?.call(this, e);
    this.dispatchEvent(e);
    this.connection.close();
    this.ended(1006, "");
  }

  private ended(code: number, reason: string): void {
    if (this.readyState === this.CLOSED) return;
    this.readyState = this.CLOSED;
    this.bufferedAmount = 0;
    const e = new CloseEvent("close", { code, reason, wasClean: code === 1000 });
    this.onclose?.call(this, e);
    this.dispatchEvent(e);
  }
}
