import { afterAll, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot } from "react-dom/client";

let useQuery: typeof import("./query.ts").useQuery;
beforeAll(async () => {
  GlobalRegistrator.register();
  ({ useQuery } = await import("./query.ts"));
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
});
afterAll(async () => {
  await GlobalRegistrator.unregister();
});

const sleep = (ms: number) => act(() => new Promise<void>((r) => setTimeout(r, ms)));

test("a poll tick while a fetch is in flight is skipped, not an abort: a slow request still lands", async () => {
  const calls: { signal: AbortSignal; resolve: (v: string) => void }[] = [];
  let data: string | undefined;
  function Probe() {
    data = useQuery("slow", (signal) => new Promise<string>((resolve) => calls.push({ signal, resolve })), { interval: 20 }).data;
    return null;
  }
  const el = document.createElement("div");
  const root = createRoot(el);
  await act(async () => root.render(<Probe />));
  try {
    expect(calls.length).toBe(1);
    // Several intervals pass with the first request unanswered.
    await sleep(110);
    expect(calls.length).toBe(1);
    expect(calls[0]!.signal.aborted).toBe(false);
    await act(async () => calls[0]!.resolve("slow answer"));
    expect(data).toBe("slow answer");
    // Once it has landed, polling resumes.
    await sleep(60);
    expect(calls.length).toBeGreaterThan(1);
  } finally {
    await act(async () => root.unmount());
  }
  // Unmount still aborts the one in flight.
  expect(calls.at(-1)!.signal.aborted).toBe(true);
});
