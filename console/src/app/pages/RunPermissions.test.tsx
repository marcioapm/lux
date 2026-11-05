import { afterAll, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
let RunPage: typeof import("./RunPage.tsx").RunPage;
let t: typeof import("../peopleTesting.tsx");
beforeAll(async () => {
  GlobalRegistrator.register();
  ({ RunPage } = await import("./RunPage.tsx"));
  t = await import("../peopleTesting.tsx");
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
});
afterAll(async () => { await GlobalRegistrator.unregister(); });
const run = { id: "run_own", tenant: "acme", name: "another-persons-run", state: "running", epoch: 1, labels: {}, owner: { kind: "user", id: "usr_bru", name: "Bruno Costa" }, spec: { image: { ref: "alpine" }, workload: { adapter: "generic" }, resources: {} }, createdAt: "2026-11-01T00:00:00Z" };

test("viewer and another member cannot act: no Terminal or run actions, note names who can", async () => {
  for (const role of ["viewer", "member"]) {
    const p = await t.mountAs(role, "/runs/run_own?tab=session", <RunPage id="run_own" />, { extra: (path) => path.startsWith("/v1/runs/run_own") ? run : undefined });
    try {
      await p.until(() => p.text().includes("another-persons-run"), "run");
      expect(p.el.querySelector(".page-head-actions")).toBeNull();
      expect(p.el.querySelector("textarea")).toBeNull();
      expect(p.text()).toContain("only Bruno Costa and admins can act on it");
      expect(p.text()).toContain("Only the owner and admins can send input");
    } finally { await p.done(); }
  }
});
