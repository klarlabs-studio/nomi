import type { NomiStreamEvent } from "./badge_events";

export type EventStreamCallbacks = {
  onEvent: (ev: NomiStreamEvent) => void;
  onConnect?: () => void;
  onDisconnect?: () => void;
};

/**
 * Long-lived GET /events/stream consumer. Mirrors `nomi tail` /
 * the Tauri Rust bridge: Authorization header from discovered token,
 * newline-delimited SSE, reconnect with exponential backoff.
 * Comments (`: ready`, `: ping`) are ignored.
 */
export class NomiEventStream {
  private abort: AbortController | undefined;
  private reconnectTimer: ReturnType<typeof setTimeout> | undefined;
  private backoffMs = 1000;
  private disposed = false;
  private connected = false;

  constructor(
    private readonly baseUrl: string,
    private readonly token: string,
    private readonly callbacks: EventStreamCallbacks,
  ) {}

  get isConnected(): boolean {
    return this.connected;
  }

  start(): void {
    if (this.disposed) return;
    void this.loop();
  }

  dispose(): void {
    this.disposed = true;
    if (this.reconnectTimer) clearTimeout(this.reconnectTimer);
    this.reconnectTimer = undefined;
    this.abort?.abort();
    this.abort = undefined;
    this.connected = false;
  }

  private async loop(): Promise<void> {
    while (!this.disposed) {
      this.abort = new AbortController();
      try {
        await this.connectOnce(this.abort.signal);
        this.backoffMs = 1000;
      } catch {
        this.markDisconnected();
        if (this.disposed) return;
        await this.sleep(this.backoffMs);
        this.backoffMs = Math.min(this.backoffMs * 2, 30_000);
      }
    }
  }

  private markDisconnected(): void {
    if (this.connected) {
      this.connected = false;
      this.callbacks.onDisconnect?.();
    }
  }

  private async connectOnce(signal: AbortSignal): Promise<void> {
    const url = `${this.baseUrl.replace(/\/$/, "")}/events/stream`;
    const res = await fetch(url, {
      headers: {
        Authorization: `Bearer ${this.token}`,
        Accept: "text/event-stream",
      },
      signal,
    });
    if (!res.ok || !res.body) {
      throw new Error(`SSE HTTP ${res.status}`);
    }
    this.connected = true;
    this.callbacks.onConnect?.();

    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buf = "";
    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += decoder.decode(value, { stream: true });
        const parts = buf.split("\n\n");
        buf = parts.pop() ?? "";
        for (const block of parts) {
          this.dispatchBlock(block);
        }
      }
    } finally {
      this.markDisconnected();
    }
    throw new Error("SSE stream ended");
  }

  /** Exposed for unit tests. */
  dispatchBlock(block: string): void {
    for (const line of block.split("\n")) {
      if (!line.startsWith("data:")) continue;
      const payload = line.slice(5).trim();
      if (!payload) continue;
      try {
        const ev = JSON.parse(payload) as NomiStreamEvent;
        if (ev && typeof ev.type === "string") {
          this.callbacks.onEvent(ev);
        }
      } catch {
        // ignore malformed frames
      }
    }
  }

  private sleep(ms: number): Promise<void> {
    return new Promise((resolve) => {
      this.reconnectTimer = setTimeout(resolve, ms);
    });
  }
}
