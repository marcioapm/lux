import { afterAll, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
let People: typeof import("./People.tsx").People;
let t: typeof import("../peopleTesting.tsx");
beforeAll(async () => { GlobalRegistrator.register(); ({ People } = await import("./People.tsx")); t = await import("../peopleTesting.tsx"); (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true; });
afterAll(async () => { await GlobalRegistrator.unregister(); });
test("People lists platform accounts with memberships and operator labels", async () => {
  const p = await t.mountAs("operator", "/people", <People />);
  try { await p.until(() => p.text().includes("Márcio Martins"), "people"); expect(p.text()).toContain("globex · Admin"); expect(p.text()).toContain("Operator"); expect(p.el.querySelectorAll("tbody tr").length).toBe(7); } finally { await p.done(); }
});
