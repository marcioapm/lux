// Server-sent events over fetch: EventSource cannot send the Authorization
// header. Parses the text/event-stream framing and reconnects from the last
// id with backoff until aborted or the server says it is done.
import { ApiError, apiFetch, type Query } from "./client.ts";

interface SSEMessage {
  event: string;
  data: string;
  id?: string;
}

export interface SSEOptions {
  query?: Query;
  tenant?: string;
  signal: AbortSignal;
  lastEventId?: string;
  onMessage: (m: SSEMessage) => void;
  /** Called when a connection ends (error: a StreamError for a server `error` event); return false to stop reconnecting. */
  onClose?: (reason: "end" | "error", error?: unknown) => boolean | void;
  /** The connection is up and healthy (not just a 200 followed by an error). */
  onOpen?: () => void;
  /** Extra query to send on reconnects (e.g. a moving cursor). */
  reconnectQuery?: () => Query;
}

/** Parse one complete event block (lines without the trailing blank line). */
function parseBlock(lines: string[]): SSEMessage | null {
  let event = "message";
  let id: string | undefined;
  const data: string[] = [];
  for (const line of lines) {
    if (line === "" || line.startsWith(":")) continue;
    const i = line.indexOf(":");
    const field = i < 0 ? line : line.slice(0, i);
    let value = i < 0 ? "" : line.slice(i + 1);
    if (value.startsWith(" ")) value = value.slice(1);
    if (field === "event") event = value;
    else if (field === "data") data.push(value);
    else if (field === "id") id = value;
  }
  if (data.length === 0) return null;
  return { event, data: data.join("\n"), id };
}

/** The server sent `event: error` and closed: a failed connection. */
class StreamError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "StreamError";
  }
}

/** The message of an `error` event: a JSON string or {"error": "..."}. */
function errorMessage(data: string): string {
  try {
    const v = JSON.parse(data) as unknown;
    if (typeof v === "string") return v;
    if (v && typeof v === "object" && typeof (v as { error?: unknown }).error === "string") return (v as { error: string }).error;
  } catch {}
  return data || "stream error";
}

/** How long a connection must stay up without an error before it counts as open. */
const SETTLE_MS = 1500;

async function readStream(res: Response, opts: SSEOptions, state: { lastId?: string }, onHealthy: () => void): Promise<void> {
  const reader = res.body?.getReader();
  if (!reader) throw new Error("no response body");
  const dec = new TextDecoder();
  let buf = "";
  let block: string[] = [];
  let failed = false;
  // A server that fails sends its error right away: count the connection as
  // open on the first real message, or once it has stayed up for a moment.
  const settle = setTimeout(() => {
    if (!failed) onHealthy();
  }, SETTLE_MS);
  const dispatch = (m: SSEMessage | null) => {
    if (!m) return;
    if (m.event === "error") {
      failed = true;
      throw new StreamError(errorMessage(m.data));
    }
    if (m.id != null) state.lastId = m.id;
    onHealthy();
    opts.onMessage(m);
  };
  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      let nl: number;
      while ((nl = buf.indexOf("\n")) >= 0) {
        let line = buf.slice(0, nl);
        buf = buf.slice(nl + 1);
        if (line.endsWith("\r")) line = line.slice(0, -1);
        if (line === "") {
          const m = parseBlock(block);
          block = [];
          dispatch(m);
        } else block.push(line);
      }
    }
    dispatch(parseBlock(block));
  } finally {
    clearTimeout(settle);
    reader.cancel().catch(() => {});
  }
}

/**
 * Open the stream and keep it open. Resolves when aborted or when onClose
 * returns false. A server `error` event counts as a failed connection: it is
 * passed to onClose and retried with backoff, which only resets once a
 * connection has proven healthy.
 */
export async function streamSSE(path: string, opts: SSEOptions): Promise<void> {
  const state = { lastId: opts.lastEventId };
  let backoff = 1000;
  while (!opts.signal.aborted) {
    let reason: "end" | "error" = "end";
    let error: unknown;
    let healthy = false;
    const onHealthy = () => {
      if (healthy || opts.signal.aborted) return;
      healthy = true;
      backoff = 1000;
      opts.onOpen?.();
    };
    try {
      const res = await apiFetch(path, {
        query: { ...(opts.query ?? {}), ...(opts.reconnectQuery?.() ?? {}) },
        tenant: opts.tenant,
        signal: opts.signal,
        headers: { Accept: "text/event-stream", ...(state.lastId != null ? { "Last-Event-ID": state.lastId } : {}) },
      });
      await readStream(res, opts, state, onHealthy);
    } catch (e) {
      if (opts.signal.aborted) return;
      reason = "error";
      error = e;
      // Auth and permission problems will not fix themselves.
      if (e instanceof ApiError && (e.status === 401 || e.status === 403 || e.status === 404)) {
        opts.onClose?.(reason, e);
        return;
      }
    }
    if (opts.signal.aborted) return;
    if (opts.onClose?.(reason, error) === false) return;
    await sleep(backoff, opts.signal);
    backoff = Math.min(backoff * 2, 15_000);
  }
}

function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const t = setTimeout(done, ms);
    function done() {
      clearTimeout(t);
      signal.removeEventListener("abort", done);
      resolve();
    }
    signal.addEventListener("abort", done);
  });
}
