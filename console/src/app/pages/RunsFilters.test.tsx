import { expect, test } from "bun:test";
import { withDom } from "../testDom.ts";
import { act } from "react";
import { createRoot } from "react-dom/client";

// The page's imports (the router) touch window at load: register the DOM first.
let Runs: typeof import("./Runs.tsx").Runs;
let RUN_SORT_KEYS: typeof import("./Runs.tsx").RUN_SORT_KEYS;
let ScopeProvider: typeof import("../scope.tsx").ScopeProvider;
let fakeApi: typeof import("../testing.ts").fakeApi;
let router: typeof import("../router.tsx");
withDom(async () => {
  ({ Runs, RUN_SORT_KEYS } = await import("./Runs.tsx"));
  ({ ScopeProvider } = await import("../scope.tsx"));
  ({ fakeApi } = await import("../testing.ts"));
  router = await import("../router.tsx");
});

const sleep = (ms: number) => act(() => new Promise<void>((r) => setTimeout(r, ms)));
const setURL = (u: string) => (window as unknown as { happyDOM: { setURL: (u: string) => void } }).happyDOM.setURL(u);

async function mount(url: string) {
  localStorage.clear();
  const fake = fakeApi((path) => (path.startsWith("/v1/runs") ? { runs: [{ id: "run_1", tenant: "acme", name: "one", state: "running", epoch: 1, labels: {}, owner: { kind: "user", id: "usr_car", name: "Carla" }, spec: { image: { ref: "alpine" }, workload: { adapter: "generic" }, resources: {} }, createdAt: "2026-11-01T00:00:00Z" }] } : path.startsWith("/v1/history") ? { samples: [] } : {}));
  setURL(`http://localhost${url}`);
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  await act(async () =>
    root.render(
      <ScopeProvider>
        <Runs />
      </ScopeProvider>,
    ),
  );
  await sleep(50);
  return {
    el,
    fake,
    header: (text: string) => [...el.querySelectorAll("thead th")].find((th) => th.textContent?.replace(/[↑↓▲▼↕]/g, "").trim() === text) as HTMLElement | undefined,
    headers: () => [...el.querySelectorAll("thead th")].map((th) => `${th.textContent?.trim()}${th.classList.contains("is-sortable") ? "*" : ""}`),
    lists: () => fake.calls.filter((c) => c.startsWith("/v1/runs?") || c === "/v1/runs"),
    done: async () => {
      await act(async () => root.unmount());
      el.remove();
      fake.restore();
    },
  };
}

// api.go runSortKeys: sort=owner is a 400. The Owner column sorts nothing.
test("Runs: the Owner header is not sortable and clicking it sends no sort=owner", async () => {
  const p = await mount("/runs");
  try {
    const owner = p.header("Owner");
    if (!owner) throw new Error(`headers: ${p.headers().join(" | ")}`);
    expect(owner!.getAttribute("aria-sort")).toBeNull();
    expect(owner!.classList.contains("is-sortable")).toBe(false);
    await act(async () => owner!.click());
    await sleep(50);
    expect(p.lists().some((c) => c.includes("sort=owner"))).toBe(false);
    // Every header that does sort is one of luxd's keys; Name still sorts.
    for (const th of p.el.querySelectorAll("thead th.is-sortable")) expect(th.textContent).not.toContain("Owner");
    expect(p.headers()).toContain("Run↕*");
    expect(RUN_SORT_KEYS.has("owner")).toBe(false);
  } finally {
    await p.done();
  }
});

/**
 * Go to url as back/forward does: the URL changes under the page, which
 * hears it from the router's location store (popstate). The store is told
 * directly too: its popstate listener is on the window of the first test
 * file that loaded the router, not necessarily this one's.
 */
async function historyGo(url: string) {
  await act(async () => {
    const u = new URL(url, "http://localhost");
    history.replaceState(null, "", "/runs?from=history");
    window.dispatchEvent(new PopStateEvent("popstate"));
    router.setSearchParams(Object.fromEntries([...new Set(["from", "mine", "state", ...u.searchParams.keys()])].map((k) => [k, u.searchParams.get(k)])));
  });
  await sleep(50);
}
const segment = (el: HTMLElement) => el.querySelector('[role="radiogroup"][aria-label="Run ownership"] [aria-checked="true"]')?.textContent;

// Mine is the URL's (back/forward too); the remembered choice only when the URL has no mine.
test("Runs: Mine follows the URL on back and forward; the remembered choice only fills a URL without mine", async () => {
  const p = await mount("/runs?mine=true");
  try {
    expect(segment(p.el)).toBe("Mine");
    expect(p.lists().at(-1)).toContain("mine=true");
    await historyGo("/runs?mine=false");
    expect(segment(p.el)).toBe("All runs");
    expect(p.lists().at(-1)).not.toContain("mine=true");
    await historyGo("/runs?mine=true");
    expect(segment(p.el)).toBe("Mine");
    expect(p.lists().at(-1)).toContain("mine=true");
    // No mine in the URL: the remembered choice (here, from clicking Mine).
    localStorage.setItem("lux.runs.mine", "true");
    await historyGo("/runs");
    expect(segment(p.el)).toBe("Mine");
    localStorage.setItem("lux.runs.mine", "false");
    await historyGo("/runs?state=running");
    expect(segment(p.el)).toBe("All runs");
    // A click writes the URL and the memory.
    const all = [...p.el.querySelectorAll('[role="radio"]')].find((b) => b.textContent === "Mine") as HTMLElement;
    await act(async () => all.click());
    await sleep(50);
    expect(new URLSearchParams(window.location.search).get("mine")).toBe("true");
    expect(localStorage.getItem("lux.runs.mine")).toBe("true");
    expect(router.matchPath("/runs", window.location.pathname)).not.toBeNull();
  } finally {
    await p.done();
  }
});
