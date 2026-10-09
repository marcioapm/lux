import { afterAll, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot } from "react-dom/client";

// The router touches window at load: register the DOM first.
let ScopeProvider: typeof import("./scope.tsx").ScopeProvider;
let useScope: typeof import("./scope.tsx").useScope;
let scoped: typeof import("./router.tsx").scoped;
beforeAll(async () => {
  GlobalRegistrator.register();
  ({ ScopeProvider, useScope } = await import("./scope.tsx"));
  ({ scoped } = await import("./router.tsx"));
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
});
afterAll(async () => {
  await GlobalRegistrator.unregister();
});

/** The scope at url, and a way to act on it. */
async function at(url: string) {
  (window as unknown as { happyDOM: { setURL: (u: string) => void } }).happyDOM.setURL(url);
  let scope: ReturnType<typeof useScope> | undefined;
  const Probe = () => {
    scope = useScope();
    return null;
  };
  const el = document.createElement("div");
  const root = createRoot(el);
  await act(async () =>
    root.render(
      <ScopeProvider>
        <Probe />
      </ScopeProvider>,
    ),
  );
  return {
    scope: () => scope!,
    done: () => act(async () => root.unmount()),
  };
}

test("cost and per read from the URL; anything unknown is the default", async () => {
  for (const [url, show, per] of [
    ["http://localhost/", "all", "auto"],
    ["http://localhost/?cost=compute&per=day", "compute", "day"],
    ["http://localhost/?cost=external&per=hour", "external", "hour"],
    ["http://localhost/?cost=machine&per=6h", "all", "auto"],
  ] as const) {
    const p = await at(url);
    expect([url, p.scope().costShow, p.scope().costPer]).toEqual([url, show, per]);
    await p.done();
  }
});

test("setting Show and Per writes the URL; the defaults leave it", async () => {
  const p = await at("http://localhost/?range=7d");
  await act(async () => p.scope().setCostShow("external"));
  await act(async () => p.scope().setCostPer("day"));
  expect(window.location.search).toBe("?range=7d&cost=external&per=day");
  expect([p.scope().costShow, p.scope().costPer]).toEqual(["external", "day"]);
  await act(async () => p.scope().setCostShow("all"));
  await act(async () => p.scope().setCostPer("auto"));
  expect(window.location.search).toBe("?range=7d");
  await p.done();
});

test("links carry cost and per with the tenant and range", () => {
  expect(scoped("/runs", "?tenant=acme&range=7d&cost=compute&per=day&tab=x")).toBe("/runs?tenant=acme&range=7d&cost=compute&per=day");
  expect(scoped("/?cost=external", "?cost=compute")).toBe("/?cost=external");
});
