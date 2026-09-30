import { afterAll, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot } from "react-dom/client";
import type { Host, LifecycleEvent } from "../../api/index.ts";

// The page's imports (the router) touch window at load: register the DOM first.
let HostPage: typeof import("./HostPage.tsx").HostPage;
let ScopeProvider: typeof import("../scope.tsx").ScopeProvider;
let ToastProvider: typeof import("@lux/design-system").ToastProvider;
let formatTimestamp: typeof import("@lux/design-system").formatTimestamp;
let fakeApi: typeof import("../testing.ts").fakeApi;
let api: typeof import("../../api/index.ts");
beforeAll(async () => {
  GlobalRegistrator.register();
  ({ HostPage } = await import("./HostPage.tsx"));
  ({ ScopeProvider } = await import("../scope.tsx"));
  ({ ToastProvider, formatTimestamp } = await import("@lux/design-system"));
  ({ fakeApi } = await import("../testing.ts"));
  api = await import("../../api/index.ts");
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
});
afterAll(async () => {
  await GlobalRegistrator.unregister();
});

const sleep = (ms: number) => act(() => new Promise<void>((r) => setTimeout(r, ms)));

const T0 = Date.parse("2026-09-01T12:00:00Z");
const iso = (s: number) => new Date(T0 + s * 1000).toISOString();

function host(over: Partial<Host> = {}): Host {
  const times = { created: null, provisionRequested: null, provisioned: null, registered: null, firstPlacement: null, lastPlacementEnded: null, drainRequested: null, terminateRequested: null, terminated: null, lost: null };
  return {
    id: "h1",
    name: "host-1",
    tenant: "acme",
    pool: "burst",
    state: "ready",
    draining: false,
    labels: {},
    capacity: { cpus: 4, memory: 1024, disk: 1024, runs: 4 },
    allocated: {},
    versions: {},
    platform: false,
    liveRuns: 0,
    ...over,
    times: { ...times, created: iso(0), ...over.times },
  };
}

const events = (from: number, n: number): LifecycleEvent[] => Array.from({ length: n }, (_, i) => ({ id: from - i, type: "host.placement_assigned", data: { run: `r${from - i}` }, count: 1, time: iso(from - i) }));

/** The host page with a fake API: the host, and its events as cursor pages of 50. */
async function render(h: Host) {
  const fake = fakeApi((path) => {
    if (path.startsWith("/v1/hosts/h1/events")) {
      const q = new URLSearchParams(path.split("?")[1]);
      return q.get("next") ? { events: events(50, 50), prev: "p2", page: "s2" } : { events: events(100, 50), next: "n1", page: "s1" };
    }
    if (path.startsWith("/v1/hosts/h1/history")) return { from: iso(0), to: iso(60), resolution: 60, samples: [] };
    if (path.startsWith("/v1/hosts/h1/cost")) return { hostId: "h1", from: iso(0), to: iso(60), basis: "list", hours: [] };
    if (path.startsWith("/v1/hosts/h1")) return h;
    if (path.startsWith("/v1/runs")) return { runs: [] };
    return {};
  });
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  await act(async () =>
    root.render(
      <ToastProvider>
        <ScopeProvider>
          <HostPage id="h1" />
        </ScopeProvider>
      </ToastProvider>,
    ),
  );
  await sleep(50);
  return {
    el,
    fake,
    eventCalls: () => fake.calls.filter((c) => c.startsWith("/v1/hosts/h1/events")),
    done: async () => {
      await act(async () => root.unmount());
      el.remove();
      fake.restore();
    },
  };
}

async function pagesThroughEvents(h: Host) {
  api.signIn("k");
  api.setRole("tenant");
  const p = await render(h);
  try {
    // The first page: sorted by time, newest first, 50 at a time; no
    // before/after (the old unpaged reads).
    const first = new URLSearchParams(p.eventCalls()[0]!.split("?")[1]);
    expect(Object.fromEntries(first)).toEqual({ sort: "time", dir: "desc", limit: "50" });
    const card = [...p.el.querySelectorAll(".card")].find((c) => c.querySelector(".card-title")?.textContent === "Events")!;
    expect(card).toBeDefined();
    expect(card.querySelectorAll("tbody tr").length).toBe(50);
    expect(card.textContent).toContain("Page 1");
    expect(card.textContent).toContain("Time, newest first");
    expect(card.textContent).not.toContain("Load older");
    // Next reads the next page by its cursor, in the same sort.
    const next = [...card.querySelectorAll("button")].find((b) => b.textContent?.includes("Next")) as HTMLButtonElement;
    await act(async () => next.click());
    await sleep(50);
    const second = new URLSearchParams(p.eventCalls().at(-1)!.split("?")[1]);
    expect(Object.fromEntries(second)).toEqual({ sort: "time", dir: "desc", limit: "50", next: "n1" });
    expect(card.textContent).toContain("Page 2");
    expect(card.querySelector("tbody tr")?.textContent).toContain(formatTimestamp(iso(50)));
  } finally {
    await p.done();
    api.signOut();
  }
}

test("a host's events are server-sorted cursor pages", async () => {
  await pagesThroughEvents(host({ times: { registered: iso(10) } as Host["times"] }));
});

test("a host whose launch failed pages its events the same way", async () => {
  await pagesThroughEvents(
    host({ state: "terminated", launch: { outcome: "failed", requestedAt: iso(0), finishedAt: iso(5), error: "InsufficientInstanceCapacity" }, times: { provisionRequested: iso(0) } as Host["times"] }),
  );
});
