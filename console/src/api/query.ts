// A small polling query hook. One in-flight request at a time, refetch on an
// interval (paused while the tab is hidden), cancelled on unmount or when
// the key changes. No cache: pages are short-lived and the API is local.
import { useCallback, useEffect, useRef, useState } from "react";
import { errorText } from "./client.ts";

export interface QueryState<T> {
  data: T | undefined;
  error: string | null;
  /** True until the first response (or error) arrives. */
  loading: boolean;
  /** A refetch is in flight. */
  fetching: boolean;
  refetch: () => Promise<void>;
  /** Replace the data locally (after an action returns the new object). */
  setData: (d: T) => void;
}

export interface QueryOptions {
  /** Poll interval in ms; 0 or undefined polls never. */
  interval?: number;
  enabled?: boolean;
  /** On a key change, keep showing the old data until the new arrives (e.g. loading another page). */
  keep?: boolean;
}

export function useQuery<T>(key: string, fn: (signal: AbortSignal) => Promise<T>, opts: QueryOptions = {}): QueryState<T> {
  const { interval = 0, enabled = true, keep = false } = opts;
  const keepRef = useRef(keep);
  keepRef.current = keep;
  const [data, setData] = useState<T | undefined>(undefined);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(enabled);
  const [fetching, setFetching] = useState(false);
  const fnRef = useRef(fn);
  fnRef.current = fn;
  const ctrl = useRef<AbortController | null>(null);
  const keyRef = useRef(key);

  const run = useCallback(async () => {
    ctrl.current?.abort();
    const c = new AbortController();
    ctrl.current = c;
    setFetching(true);
    try {
      const d = await fnRef.current(c.signal);
      if (c.signal.aborted) return;
      setData(d);
      setError(null);
    } catch (e) {
      if (c.signal.aborted) return;
      setError(errorText(e));
    } finally {
      if (!c.signal.aborted) {
        setLoading(false);
        setFetching(false);
      }
    }
  }, []);

  useEffect(() => {
    if (!enabled) {
      setLoading(false);
      return;
    }
    if (keyRef.current !== key) {
      keyRef.current = key;
      if (!keepRef.current) {
        setData(undefined);
        setError(null);
        setLoading(true);
      }
    }
    void run();
    let timer: ReturnType<typeof setInterval> | undefined;
    const start = () => {
      if (interval > 0 && timer == null) timer = setInterval(() => void run(), interval);
    };
    const stop = () => {
      if (timer != null) clearInterval(timer);
      timer = undefined;
    };
    const onVis = () => {
      if (document.hidden) stop();
      else {
        void run();
        start();
      }
    };
    start();
    document.addEventListener("visibilitychange", onVis);
    return () => {
      stop();
      document.removeEventListener("visibilitychange", onVis);
      ctrl.current?.abort();
    };
  }, [key, interval, enabled, run]);

  return { data, error, loading, fetching, refetch: run, setData };
}

/** Re-render every `ms` so relative times stay fresh. Returns Date.now(). */
export function useNow(ms = 10_000): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), ms);
    return () => clearInterval(t);
  }, [ms]);
  return now;
}
