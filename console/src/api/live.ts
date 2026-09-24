// The tab's one Run event stream (GET /v1/events, SSE). useLiveStream opens
// it for the current tenant scope (mount once); everything else subscribes:
// queries refetch on matching events (useQuery's `live` option), the activity
// feed reads the newest events, the top bar shows whether it is up.
import { useEffect, useSyncExternalStore } from "react";
import { ApiError, errorText } from "./client.ts";
import { streamSSE } from "./sse.ts";
import type { FeedEvent } from "./types.ts";

/** off: given up (a 401, 403 or 404 will not fix itself). */
export type LiveStatus = "off" | "connecting" | "live" | "reconnecting";

export interface LiveState {
  status: LiveStatus;
  /** Why the last connection failed; cleared once live again. */
  error: string | null;
  /** The newest events, newest first (backfilled on connect). */
  recent: FeedEvent[];
}

const RECENT = 50;

let state: LiveState = { status: "connecting", error: null, recent: [] };
const watchers = new Set<() => void>();
const handlers = new Set<(e: FeedEvent) => void>();

function set(patch: Partial<LiveState>) {
  state = { ...state, ...patch };
  watchers.forEach((w) => w());
}

function watch(cb: () => void): () => void {
  watchers.add(cb);
  return () => watchers.delete(cb);
}

/** Open the stream for a tenant scope (undefined: all the key sees); reopens when it changes. */
export function useLiveStream(tenant: string | undefined): void {
  useEffect(() => {
    const ctrl = new AbortController();
    set({ status: "connecting", error: null, recent: [] });
    void streamSSE("/events", {
      query: { follow: true, last: RECENT },
      tenant,
      signal: ctrl.signal,
      onOpen: () => set({ status: "live", error: null }),
      onMessage: (m) => {
        if (ctrl.signal.aborted || m.event !== "lux") return;
        let e: FeedEvent;
        try {
          e = JSON.parse(m.data) as FeedEvent;
        } catch {
          return;
        }
        if (state.recent.some((x) => x.id === e.id)) return;
        set({ recent: [e, ...state.recent].slice(0, RECENT) });
        handlers.forEach((h) => h(e));
      },
      onClose: (_reason, err) => {
        if (ctrl.signal.aborted) return;
        // A clean end keeps the last error until a healthy reconnect.
        const final = err instanceof ApiError && (err.status === 401 || err.status === 403 || err.status === 404);
        set({ status: final ? "off" : "reconnecting", ...(err ? { error: errorText(err) } : {}) });
      },
    });
    return () => {
      ctrl.abort();
      set({ status: "connecting" });
    };
  }, [tenant]);
}

/** Call `h` for every event as it arrives. Returns the unsubscribe. */
export function onLiveEvent(h: (e: FeedEvent) => void): () => void {
  handlers.add(h);
  return () => handlers.delete(h);
}

export function useLiveStatus(): LiveStatus {
  return useSyncExternalStore(watch, () => state.status);
}

export function useLiveState(): LiveState {
  return useSyncExternalStore(watch, () => state);
}
