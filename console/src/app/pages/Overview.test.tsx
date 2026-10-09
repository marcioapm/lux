import { afterAll, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot } from "react-dom/client";
import type { Sample } from "../../api/index.ts";

// The page's imports (the router) touch window at load: register the DOM first.
let Overview: typeof import("./Overview.tsx").Overview;
let storedSeries: typeof import("./Overview.tsx").storedSeries;
let ScopeProvider: typeof import("../scope.tsx").ScopeProvider;
let fakeApi: typeof import("../testing.ts").fakeApi;
let api: typeof import("../../api/index.ts");
beforeAll(async () => {
  GlobalRegistrator.register();
  ({ Overview, storedSeries } = await import("./Overview.tsx"));
  ({ ScopeProvider } = await import("../scope.tsx"));
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

const SAMPLES: Sample[] = [
  { at: iso(0), storedVolume: 100, storedOutput: 7, storedArtifact: 0, storedContext: 40 },
  { at: iso(60), storedVolume: 120, storedOutput: 9, storedArtifact: 3, storedContext: 40 },
];

test("Stored: one series per charted kind, values from its own field; storedContext is not charted", () => {
  const s = storedSeries(SAMPLES);
  expect(s.series.map((x) => x.label)).toEqual(["Snapshots", "Output", "Artifacts"]);
  expect(s.series.map((x) => x.color)).toEqual(["var(--chart-3)", "var(--chart-7)", "var(--chart-4)"]);
  expect(s.x).toEqual([T0 / 1000, T0 / 1000 + 60]);
  expect(s.ys).toEqual([
    [100, 120],
    [7, 9],
    [0, 3],
  ]);
  // The stack's total leaves out the 40 bytes of storedContext.
  expect(s.ys.reduce((t, y) => t + (y[1] ?? 0), 0)).toBe(132);
});

test("Stored: a sample without the fields is a gap, not zero", () => {
  expect(storedSeries([{ at: iso(0) }]).ys).toEqual([[null], [null], [null]]);
});

/** The Overview, as role at url. History answers one sample: uPlot needs a canvas happy-dom lacks, and draws from two. */
async function render(role: "tenant" | "operator", url: string, answer?: (path: string) => unknown) {
  api.signIn("k");
  api.setRole(role);
  const fake = fakeApi((path) => {
    const a = answer?.(path);
    if (a !== undefined) return a;
    if (path.startsWith("/v1/history")) return { from: iso(0), to: iso(60), resolution: 60, samples: SAMPLES.slice(0, 1) };
    if (path.startsWith("/v1/status")) return { runs: {}, busy: 0, idle: 0, queued: 0, startLatency: { n: 0 }, hosts: {}, capacity: { cpus: 0, memory: 0 }, allocated: { cpus: 0, memory: 0 } };
    return {};
  });
  (window as unknown as { happyDOM: { setURL: (u: string) => void } }).happyDOM.setURL(url);
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  await act(async () =>
    root.render(
      <ScopeProvider>
        <Overview />
      </ScopeProvider>,
    ),
  );
  await sleep(50);
  return {
    fake,
    el,
    card: () => [...el.querySelectorAll(".card")].find((c) => c.querySelector(".card-title")?.textContent === "Stored"),
    done: async () => {
      await act(async () => root.unmount());
      el.remove();
      fake.restore();
      api.signOut();
    },
  };
}

for (const c of [
  { name: "a tenant's own view", role: "tenant" as const, url: "http://localhost/", tenant: undefined },
  { name: "the whole system", role: "operator" as const, url: "http://localhost/", tenant: undefined },
  { name: "an operator narrowed to a tenant", role: "operator" as const, url: "http://localhost/?tenant=acme", tenant: "acme" },
]) {
  test(`the Stored card in ${c.name}`, async () => {
    const p = await render(c.role, c.url);
    try {
      const card = p.card();
      expect(card).toBeDefined();
      expect(card!.textContent).toContain("in S3, by kind");
      const legend = [...card!.querySelectorAll(".tschart-legend-item")].map((b) => b.textContent);
      expect(legend).toEqual(["Snapshots", "Output", "Artifacts"]);
      const history = p.fake.calls.find((u) => u.startsWith("/v1/history"))!;
      expect(new URL(history, "http://localhost").searchParams.get("tenant") ?? undefined).toBe(c.tenant);
    } finally {
      await p.done();
    }
  });
}

const costCalls = (calls: string[]) => calls.filter((u) => u.startsWith("/v1/costs?")).map((u) => new URL(u, "http://x").searchParams);
const historyCall = (calls: string[]) => new URL(calls.find((u) => u.startsWith("/v1/history"))!, "http://x").searchParams;

test("the Cost panel's filters and Every reach every cost request, and Every the history's res", async () => {
  const p = await render("tenant", "http://localhost/?range=7d&every=hour&label=app%3Da&label=app%3Db&nolabel=phase");
  try {
    const costs = costCalls(p.fake.calls);
    expect(costs.length).toBeGreaterThan(0);
    for (const q of costs) {
      expect(q.getAll("label")).toEqual(["app=a", "app=b"]);
      expect(q.getAll("nolabel")).toEqual(["phase"]);
    }
    // Family is the default breakdown: one request sends interval, the family + series one.
    expect(costs.filter((q) => q.has("interval")).map((q) => q.get("interval"))).toEqual(["hour"]);
    const labels = p.fake.calls.find((u) => u.startsWith("/v1/costs/labels"));
    expect(labels).toBeDefined();
    expect(historyCall(p.fake.calls).get("res")).toBe("3600");
  } finally {
    await p.done();
  }
});

test("Every Auto sends no history res", async () => {
  const p = await render("tenant", "http://localhost/?range=7d");
  try {
    expect(historyCall(p.fake.calls).has("res")).toBe(false);
  } finally {
    await p.done();
  }
});

test("every cost summary that lists values asks luxd to fold to its top N, ranked by Show", async () => {
  const p = await render("operator", "http://localhost/?by=key&cost=external");
  try {
    const costs = costCalls(p.fake.calls);
    const of = (g: string[]) => costs.filter((q) => JSON.stringify(q.getAll("group")) === JSON.stringify(g));
    for (const [g, top] of [
      [["key"], "7"],
      [["key", "family"], "7"],
      [["run", "family"], "10"],
      [["tenant", "family"], "10"],
    ] as const) {
      const qs = of([...g]);
      expect(qs.length).toBe(1);
      expect([qs[0]!.get("top"), qs[0]!.get("rank")]).toEqual([top, "external"]);
    }
    // The breakdown's series applies Show by family; its split keeps every family.
    expect(of(["key"])[0]!.get("nofamily")).toBe("compute");
    expect(of(["key", "family"])[0]!.has("nofamily")).toBe(false);
    // No summary is grouped by a value and run any more: Runs per value come with the fold.
    expect(costs.some((q) => q.getAll("group").length === 2 && q.getAll("group")[1] === "run")).toBe(false);
  } finally {
    await p.done();
  }
});

test("Break down by Label names its key in the URL once the keys are known", async () => {
  let release!: () => void;
  const keysArrive = new Promise<void>((r) => (release = r));
  const p = await render("tenant", "http://localhost/", (path) => (path.startsWith("/v1/costs/labels") ? keysArrive.then(() => ({ from: iso(0), to: iso(60), keys: [{ key: "team", runs: 3 }, { key: "app", runs: 2 }] })) : undefined));
  try {
    const radio = (name: string) => [...p.el.querySelectorAll<HTMLButtonElement>('[role="radiogroup"][aria-label="Break down by"] [role="radio"]')].find((b) => b.textContent === name)!;
    const before = history.length;
    // Before the keys arrive: by=label, no key yet.
    await act(async () => radio("Label").click());
    expect(new URLSearchParams(location.search).get("by")).toBe("label");
    expect(history.length).toBe(before);
    // Then the default key, app, replaces it (no history entry).
    await act(async () => release());
    await sleep(20);
    expect(location.search).toContain("by=label%3Aapp");
    expect(history.length).toBe(before);
    // Once known, a click writes the key at once.
    await act(async () => radio("Family").click());
    expect(new URLSearchParams(location.search).get("by")).toBeNull();
    await act(async () => radio("Label").click());
    expect(location.search).toContain("by=label%3Aapp");
  } finally {
    await p.done();
  }
});

test("a cost request that fails shows the error, never also 'No cost'", async () => {
  const tooLarge = () => new Response(JSON.stringify({ error: { code: "too_large", message: "cost response exceeds 10000 rows" } }), { status: 413, headers: { "Content-Type": "application/json" } });
  const p = await render("tenant", "http://localhost/?by=key", (path) => {
    if (!path.startsWith("/v1/costs?")) return undefined;
    const q = new URL(path, "http://x").searchParams;
    if (q.getAll("group").includes("key") && q.has("interval")) return tooLarge();
    return { from: iso(0), to: iso(3600), basis: "list", totals: q.getAll("group").join() === "family" ? [{ group: { family: "compute" }, currency: "USD", amount: "1" }] : [], series: [] };
  });
  try {
    const panel = p.el.querySelector(".cost-panel")!;
    expect(panel.textContent).toContain("10000 rows");
    expect(panel.querySelector(".cost-charts")!.textContent).not.toContain("cost in this range");
  } finally {
    await p.done();
  }
});
